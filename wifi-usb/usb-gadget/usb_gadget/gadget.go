package usb_gadget

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	GadgetDir = "/sys/kernel/config/usb_gadget/g1"
	LUNDir    = "/sys/kernel/config/usb_gadget/g1/functions/mass_storage.usb0/lun.0"
	ValidExts = []string{".nc", ".cnc", ".tap", ".wiz", ".txt", ".eia"}
)

// IsBound returns true if the gadget is currently bound to a UDC.
func IsBound() bool {
	data, err := os.ReadFile(filepath.Join(GadgetDir, "UDC"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}

// Detach unbinds the gadget from the UDC.
func Detach() {
	if err := os.WriteFile(filepath.Join(GadgetDir, "UDC"), []byte("\n"), 0644); err != nil {
		log.Printf("Failed to detach UDC (if unbound, this is expected): %v", err)
	} else {
		log.Println("Gadget detached.")
	}
}

// Update rebuilds the disk image from the source directory and binds it to the gadget.
func Update(sourceDir, imgFile string) {
	log.Println("Updating USB gadget image...")

	// 1. Get UDC
	udcCmd := exec.Command("sh", "-c", "ls /sys/class/udc | head -n 1")
	out, _ := udcCmd.Output()
	udc := strings.TrimSpace(string(out))

	// 2. Detach
	Detach()

	// 3. Cleanup existing loops
	exec.Command("sh", "-c", fmt.Sprintf("losetup -j %s | cut -d: -f1 | xargs -r losetup -d", imgFile)).Run()

	// 4. Fallocate
	cmd := exec.Command("fallocate", "-l", "128M", imgFile)
	if outF, err := cmd.CombinedOutput(); err != nil {
		log.Printf("fallocate failed: %v, out: %s", err, string(outF))
	}

	// 5. FAT32
	cmd = exec.Command("mkfs.vfat", "-F32", "-n", "DROPPORTAL", imgFile)
	if outF, err := cmd.CombinedOutput(); err != nil {
		log.Printf("mkfs.vfat failed: %v, out: %s", err, string(outF))
	}

	// 6. Mount and Rsync
	tmpDir, err := os.MkdirTemp("/dev/shm", "drop_portal_mnt_*")
	if err == nil {
		defer os.Remove(tmpDir)
		cmd = exec.Command("mount", "-o", "loop", imgFile, tmpDir)
		if outM, errM := cmd.CombinedOutput(); errM == nil {
			cmd = exec.Command("rsync", "-rltD", "--delete", "--no-owner", "--no-group", sourceDir+"/", tmpDir+"/")
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

	// 7. Bind image to LUN
	err = os.WriteFile(filepath.Join(LUNDir, "file"), []byte(imgFile+"\n"), 0644)
	if err != nil {
		log.Printf("Failed to bind image to gadget: %v", err)
	} else {
		log.Println("Gadget image updated successfully.")
	}

	// 8. Bind UDC
	if udc != "" {
		err = os.WriteFile(filepath.Join(GadgetDir, "UDC"), []byte(udc+"\n"), 0644)
		if err != nil {
			log.Printf("Failed to bind UDC: %v", err)
		} else {
			log.Printf("Gadget bound to %s", udc)
		}
	}
}

// RestartService restarts the usb-gadget system service.
func RestartService() error {
	cmd := exec.Command("/etc/init.d/usb-gadget", "restart")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart failed: %w, out: %s", err, string(out))
	}
	return nil
}
