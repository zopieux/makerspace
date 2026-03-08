package main

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed index.html
var content embed.FS

var (
	validExts = []string{".nc", ".cnc", ".tap", ".wiz", ".txt", ".eia"}
)

const (
	portalDir = "/dev/shm/drop-portal"
	imgFile   = "/dev/shm/drop-portal.img"
	gadgetDir = "/sys/kernel/config/usb_gadget/g1"
	lunDir    = gadgetDir + "/functions/mass_storage.usb0/lun.0"
)

func sanitizeUsername(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	if len(s) > 31 {
		s = s[:31]
	}
	s = regexp.MustCompile("[^a-z0-9_-]").ReplaceAllString(s, "")
	return s
}

func sanitizeFilename(f string) string {
	s := regexp.MustCompile("[^a-zA-Z0-9_.-]").ReplaceAllString(f, "_")
	if len(s) > 255 {
		s = s[:255]
	}
	return s
}

func updateGadget() {
	log.Println("Updating USB gadget image...")

	// 1. Unbind UDC
	udcCmd := exec.Command("sh", "-c", "ls /sys/class/udc | head -n 1")
	out, _ := udcCmd.Output()
	udc := strings.TrimSpace(string(out))

	detachGadget()

	img := imgFile
	exec.Command("sh", "-c", fmt.Sprintf("losetup -j %s | cut -d: -f1 | xargs -r losetup -d", img)).Run()

	// 3. Fallocate
	cmd := exec.Command("fallocate", "-l", "128M", img)
	if outF, err := cmd.CombinedOutput(); err != nil {
		log.Printf("fallocate failed: %v, out: %s", err, string(outF))
	}

	// 4. FAT32
	cmd = exec.Command("mkfs.vfat", "-F32", "-n", "DROPPORTAL", img)
	if outF, err := cmd.CombinedOutput(); err != nil {
		log.Printf("mkfs.vfat failed: %v, out: %s", err, string(outF))
	}

	// 5. Mount and Rsync
	tmpDir, err := os.MkdirTemp("/dev/shm", "drop_portal_mnt_*")
	if err == nil {
		defer os.Remove(tmpDir)
		cmd = exec.Command("mount", "-o", "loop", img, tmpDir)
		if outM, errM := cmd.CombinedOutput(); errM == nil {
			cmd = exec.Command("rsync", "-rltD", "--delete", "--no-owner", "--no-group", portalDir+"/", tmpDir+"/")
			if outR, errR := cmd.CombinedOutput(); errR != nil {
				log.Printf("rsync failed: %v, out: %s", errR, string(outR))
			}
			exec.Command("sync").Run()
			exec.Command("umount", tmpDir).Run()
		} else {
			log.Printf("mount failed: %v, out: %s", errM, string(outM))
		}
	} else {
		log.Printf("Failed to create temp dir for mounting: %v", err)
	}
	
	err = os.WriteFile(filepath.Join(lunDir, "file"), []byte(img+"\n"), 0644)
	if err != nil {
		log.Printf("Failed to bind image to gadget: %v", err)
	} else {
		log.Println("Gadget image updated successfully.")
	}

	if udc != "" {
		err = os.WriteFile(gadgetDir+"/UDC", []byte(udc+"\n"), 0644)
		if err != nil {
			log.Printf("Failed to bind UDC: %v", err)
		} else {
			log.Printf("Gadget bound to %s", udc)
		}
	}
}

func cleanupLoop() {
	for {
		time.Sleep(1 * time.Hour)
		log.Printf("Running cleanup of old files in %s...", portalDir)

		threshold := time.Now().Add(-30 * 24 * time.Hour)
		filepath.Walk(portalDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.HasPrefix(info.Name(), "[ ") {
				return nil
			}
			if info.ModTime().Before(threshold) {
				log.Printf("Deleting old file: %s", path)
				os.Remove(path)
			}
			return nil
		})

		// Remove user directories if they have become empty
		entries, err := os.ReadDir(portalDir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					dirPath := filepath.Join(portalDir, entry.Name())
			subEntries, err := os.ReadDir(dirPath)
					if err == nil && len(subEntries) == 0 {
						if err := os.Remove(dirPath); err == nil {
							log.Printf("Deleted empty user directory: %s", dirPath)
						}
					}
				}
			}
		}
	}
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	err := r.ParseMultipartForm(100 << 20) // 100MB max limit
	if err != nil {
		http.Error(w, "File too large or malformed", http.StatusBadRequest)
		return
	}

	uname := sanitizeUsername(r.FormValue("username"))
	if uname == "" {
		http.Error(w, "Invalid username", http.StatusBadRequest)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Could not get file", http.StatusBadRequest)
		return
	}
	defer file.Close()		

	fname := sanitizeFilename(handler.Filename)
	ext := strings.ToLower(filepath.Ext(fname))

	validExt := false
	for _, e := range validExts {
		if ext == e {
			validExt = true
			break
		}
	}
	if !validExt {
		http.Error(w, "Invalid file extension", http.StatusBadRequest)
		return
	}

	userDir := filepath.Join(portalDir, uname)
	if err := os.MkdirAll(userDir, 0755); err != nil {
		http.Error(w, "Server error creating directory", http.StatusInternalServerError)
		return
	}

	destPath := filepath.Join(userDir, fname)
	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Server error saving file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()

	if _, err := io.Copy(dest, file); err != nil {
		http.Error(w, "Error writing file", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully saved file %s for user %s", destPath, uname)

	go updateGadget()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("File uploaded successfully!"))
}

func detachGadget() {
	if err := os.WriteFile(gadgetDir+"/UDC", []byte("\n"), 0644); err != nil {
		log.Printf("Failed to detach UDC (if unbound, this is expected): %v", err)
	} else {
		log.Println("Gadget detached.")
	}
}

func handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Println("Admin: Restarting Gadget Service")
	cmd := exec.Command("/etc/init.d/usb-gadget", "restart")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("Restart failed: %v, out: %s", err, string(out))
		http.Error(w, "Restart failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Gadget Restarted!"))
}

func handleAdminDetach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Println("Admin: Detaching Gadget")
	detachGadget()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Gadget Detached!"))
}

func handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Println("Admin: Forcing Update")
	go updateGadget()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Gadget Update Started!"))
}

func main() {
	if err := os.MkdirAll(portalDir, 0755); err != nil {
		log.Fatalf("Failed to create %s: %v", portalDir, err)
	}

	if ssid := os.Getenv("AP_SSID"); ssid != "" {
		markerFile := filepath.Join(portalDir, fmt.Sprintf("[ CONNECT TO '%s' WIFI TO UPLOAD ].txt", ssid))
		if err := os.WriteFile(markerFile, []byte(""), 0644); err != nil {
			log.Printf("Failed to write marker file: %v", err)
		}
	}

	go updateGadget()

	go cleanupLoop()

	tmpl := template.Must(template.ParseFS(content, "index.html"))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if r.Method == http.MethodGet {
			tmpl.Execute(w, nil)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/admin/restart", handleAdminRestart)
	http.HandleFunc("/admin/detach", handleAdminDetach)
	http.HandleFunc("/admin/update", handleAdminUpdate)

	log.Println("Starting server on :80")
	if err := http.ListenAndServe(":80", nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
