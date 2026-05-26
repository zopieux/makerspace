package usb

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	GadgetDir         = "/sys/kernel/config/usb_gadget/g1"
	StringsPath       = GadgetDir + "/strings/0x409"
	ConfigsPath       = GadgetDir + "/configs/c.1"
	ConfigStringsPath = ConfigsPath + "/strings/0x409"
	FunctionsPath     = GadgetDir + "/functions/mass_storage.usb0"
	LUNDir            = FunctionsPath + "/lun.0"
)

var (
	ValidExts = []string{"nc", "cnc", "tap", "wiz", "txt", "eia"}
)

type GadgetConfig struct {
	VendorID        string
	ProductID       string
	SerialNumber    string
	Manufacturer    string
	Product         string
	ConfigName      string
	MaxPower        int
	InquiryVendor   string
	InquiryProduct  string
	InquiryRevision string
}

func writeSysfs(path, val string) error {
	return os.WriteFile(path, []byte(val+"\n"), 0644)
}

func GadgetInit(cfg GadgetConfig) error {
	log.Println("Setting up USB Gadget")

	// Wait for UDC
	udcFound := false
	for i := 0; i < 20; i++ {
		if udc := getUDC(); udc != "" {
			udcFound = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !udcFound {
		return fmt.Errorf("no UDC found")
	}

	if err := os.MkdirAll(GadgetDir, 0755); err != nil {
		return fmt.Errorf("failed to create gadget dir: %w", err)
	}

	writeSysfs(filepath.Join(GadgetDir, "idVendor"), cfg.VendorID)
	writeSysfs(filepath.Join(GadgetDir, "idProduct"), cfg.ProductID)
	writeSysfs(filepath.Join(GadgetDir, "bcdDevice"), "0x0100")
	writeSysfs(filepath.Join(GadgetDir, "bcdUSB"), "0x0200")
	writeSysfs(filepath.Join(GadgetDir, "bDeviceClass"), "0xEF")
	writeSysfs(filepath.Join(GadgetDir, "bDeviceSubClass"), "0x02")
	writeSysfs(filepath.Join(GadgetDir, "bDeviceProtocol"), "0x01")

	os.MkdirAll(StringsPath, 0755)
	writeSysfs(filepath.Join(StringsPath, "serialnumber"), cfg.SerialNumber)
	writeSysfs(filepath.Join(StringsPath, "manufacturer"), cfg.Manufacturer)
	writeSysfs(filepath.Join(StringsPath, "product"), cfg.Product)

	os.MkdirAll(ConfigStringsPath, 0755)
	writeSysfs(filepath.Join(ConfigStringsPath, "configuration"), cfg.ConfigName)
	writeSysfs(filepath.Join(ConfigsPath, "MaxPower"), fmt.Sprintf("%d", cfg.MaxPower))

	os.MkdirAll(FunctionsPath, 0755)
	writeSysfs(filepath.Join(FunctionsPath, "stall"), "1")
	writeSysfs(filepath.Join(LUNDir, "removable"), "1")
	writeSysfs(filepath.Join(LUNDir, "ro"), "0")

	inquiryStr := fmt.Sprintf("%-8.8s%-16.16s%-4.4s", cfg.InquiryVendor, cfg.InquiryProduct, cfg.InquiryRevision)
	writeSysfs(filepath.Join(LUNDir, "inquiry_string"), inquiryStr)

	// Link function
	symlinkPath := filepath.Join(ConfigsPath, "mass_storage.usb0")
	if _, err := os.Stat(symlinkPath); os.IsNotExist(err) {
		os.Symlink(FunctionsPath, symlinkPath)
	}

	return nil
}

func GadgetClose() {
	log.Println("Stopping USB Gadget")
	// Unbind UDC
	writeSysfs(filepath.Join(GadgetDir, "UDC"), "")

	// Remove function links from config
	os.Remove(filepath.Join(ConfigsPath, "mass_storage.usb0"))

	// Remove functions
	os.Remove(FunctionsPath)

	// Remove config strings, config
	os.Remove(ConfigStringsPath)
	os.Remove(ConfigsPath)
	os.Remove(StringsPath)
	os.Remove(GadgetDir)
}

var currentUblk *UblkServer

// IsBound returns true if the gadget is currently bound to a UDC.
func IsBound() bool {
	data, err := os.ReadFile(filepath.Join(GadgetDir, "UDC"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}

func Detach() {
	if err := os.WriteFile(filepath.Join(GadgetDir, "UDC"), []byte("\n"), 0644); err != nil {
		log.Printf("Failed to detach UDC (if unbound, this is expected): %v", err)
	} else {
		log.Println("Gadget detached.")
	}

	// Unbind the backing file from the LUN to release the block device
	if err := os.WriteFile(filepath.Join(LUNDir, "file"), []byte("\n"), 0644); err != nil {
		log.Printf("Failed to clear LUN file (if unbound, this is expected): %v", err)
	} else {
		log.Println("LUN file cleared.")
	}

	if currentUblk != nil {
		devPath := currentUblk.DevPath()

		// Poll to ensure the kernel has fully released the block device
		// before we attempt to close (and delete) it via go-ublk.
		for i := 0; i < 50; i++ {
			f, err := os.OpenFile(devPath, os.O_RDONLY|os.O_EXCL, 0)
			if err == nil {
				f.Close()
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		if err := currentUblk.Close(); err != nil {
			log.Printf("Failed to close ublk properly: %v", err)
		}
		currentUblk = nil
	}
}

func getUDC() string {
	entries, err := os.ReadDir("/sys/class/udc")
	if err == nil && len(entries) > 0 {
		return entries[0].Name()
	}
	return ""
}

func bindDevice(devPath string, udc string) {
	err := os.WriteFile(filepath.Join(LUNDir, "file"), []byte(devPath+"\n"), 0644)
	if err != nil {
		log.Printf("Failed to bind device to gadget: %v", err)
	} else {
		log.Printf("Gadget device bound successfully (%s).", devPath)
	}

	if udc != "" {
		err = os.WriteFile(filepath.Join(GadgetDir, "UDC"), []byte(udc+"\n"), 0644)
		if err != nil {
			log.Printf("Failed to bind UDC: %v", err)
		} else {
			log.Printf("Gadget bound to %s", udc)
		}
	}
}

// LosetupUpdate rebuilds the disk image from the source directory and binds it to the gadget using losetup.
func LosetupUpdate(sourceDir, imgFile string) {
	log.Println("Updating USB gadget image using losetup...")
	udc := getUDC()
	Detach()

	// Cleanup existing loops
	exec.Command("sh", "-c", fmt.Sprintf("losetup -j %s | cut -d: -f1 | xargs -r losetup -d", imgFile)).Run()

	// Fallocate
	cmd := exec.Command("fallocate", "-l", "128M", imgFile)
	if outF, err := cmd.CombinedOutput(); err != nil {
		log.Printf("fallocate failed: %v, out: %s", err, string(outF))
	}

	// FAT32
	cmd = exec.Command("mkfs.vfat", "-F32", "-n", "DROPPORTAL", imgFile)
	if outF, err := cmd.CombinedOutput(); err != nil {
		log.Printf("mkfs.vfat failed: %v, out: %s", err, string(outF))
	}

	// Mount and Rsync
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

	bindDevice(imgFile, udc)
}

// UblkUpdate creates a VirtualFAT and serves it using ublk, binding it to the gadget.
func UblkUpdate(sourceDir, label string) *UblkServer {
	log.Println("Updating USB gadget image via UBLK...")
	udc := getUDC()
	Detach()

	// Restart UBLK Server
	if currentUblk != nil {
		currentUblk.Close()
		currentUblk = nil
	}

	srv, err := ServeVirtualFAT(sourceDir, label)
	if err != nil {
		log.Printf("Failed to start UBLK server: %v", err)
		return nil
	}
	currentUblk = srv

	devPath := srv.DevPath()
	log.Printf("Binding via UBLK to %s on %s", devPath, udc)
	// Wait for dev to appear.
	bound := false
	for range 100 {
		if _, err := os.Stat(devPath); err == nil {
			bound = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !bound {
		log.Printf("Failed to bind via UBLK to %s on %s: device never appeared", devPath, udc)
		return srv
	}
	bindDevice(devPath, udc)
	return srv
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
