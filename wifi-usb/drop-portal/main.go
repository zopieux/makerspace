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
	portalDir = "/tmp/drop-portal"
	imgFile   = "/tmp/drop-portal.img"
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

func calculateBaseSize() int64 {
	var size int64
	err := filepath.Walk(portalDir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.HasSuffix(info.Name(), ".img") {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return 32 * 1024 * 1024 // Fallback 32MB
	}
	return size + 16*1024*1024 // Current size + 16MB overhead
}

func updateGadget() {
	log.Println("Updating USB gadget image...")

	err := os.WriteFile(filepath.Join(lunDir, "forced_eject"), []byte("1\n"), 0644)
	if err != nil {
		log.Printf("Failed to force eject gadget (if gadget not configured, this is expected): %v", err)
	}

	img := imgFile
	size := calculateBaseSize()

	f, err := os.Create(img)
	if err != nil {
		log.Printf("Failed to create image: %v", err)
		return
	}
	f.Truncate(size)
	f.Close()

	cmd := exec.Command("mkfs.vfat", "-n", "DROPPORTAL", img)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("mkfs.vfat failed: %v, out: %s", err, string(out))
	}

	// Copy recursively, ignoring the image itself
	cmd = exec.Command("sh", "-c", fmt.Sprintf("cd /tmp && mcopy -s -i %s drop-portal/* ::/", img))
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("mcopy failed: %v, out: %s", err, string(out))
	}

	time.Sleep(1 * time.Second)

	err = os.WriteFile(filepath.Join(lunDir, "file"), []byte(img+"\n"), 0644)
	if err != nil {
		log.Printf("Failed to re-attach image to gadget: %v", err)
	} else {
		log.Println("Gadget image updated successfully.")
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

	log.Println("Starting server on :80")
	if err := http.ListenAndServe(":80", nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
