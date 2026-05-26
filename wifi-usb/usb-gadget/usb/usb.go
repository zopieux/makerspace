package usb

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/warthog618/go-gpiocdev"
)

const (
	usbSelectPin     = 0
	usbEnablePin     = 1
	gpioWantedPrefix = "pinctrl-bcm2"
)

type USBMode int

const (
	USBModeHost USBMode = iota
	USBModePeripheral

	usbEnableEnabled  = 0
	usbEnableDisabled = 1

	usbSelectHost       = 0
	usbSelectPeripheral = 1
)

func (m USBMode) String() string {
	switch m {
	case USBModeHost:
		return "host"
	case USBModePeripheral:
		return "peripheral"
	default:
		return "unknown"
	}
}

var (
	gChip       *gpiocdev.Chip
	gSelectLine *gpiocdev.Line
	gEnableLine *gpiocdev.Line
	gpioMu      sync.Mutex
)

func getGpio() error {
	gpioMu.Lock()
	defer gpioMu.Unlock()

	// Singleton.
	if gChip != nil {
		return nil
	}

	chip, err := findGpioChip()
	if err != nil {
		return err
	}

	// Request both lines as outputs. Disable USB initially.
	enableLine, err := chip.RequestLine(usbEnablePin, gpiocdev.AsOutput(usbEnableDisabled))
	if err != nil {
		chip.Close()
		return fmt.Errorf("failed to request usb-enable GPIO line %d: %w", usbEnablePin, err)
	}

	selectLine, err := chip.RequestLine(usbSelectPin, gpiocdev.AsOutput(usbSelectHost))
	if err != nil {
		enableLine.Close()
		chip.Close()
		return fmt.Errorf("failed to request usb-select GPIO line %d: %w", usbSelectPin, err)
	}

	gChip = chip
	gEnableLine = enableLine
	gSelectLine = selectLine
	return nil
}

// setUSBSelect toggles the GPIO pin to switch the TS3USB221 USB selector.
func setUSBSelect(mode USBMode) error {
	if err := getGpio(); err != nil {
		return err
	}

	modeVal := map[USBMode]int{
		USBModeHost:       usbSelectHost,
		USBModePeripheral: usbSelectPeripheral,
	}[mode]

	// Disable USB to avoid glitching the USB stack.
	if err := gEnableLine.SetValue(usbEnableDisabled); err != nil {
		return fmt.Errorf("failed to disable USB: %w", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Set usb-select pin to target value, while bus is disabled.
	if err := gSelectLine.SetValue(modeVal); err != nil {
		return fmt.Errorf("failed to set usb-select: %w", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Re-enable USB.
	if err := gEnableLine.SetValue(usbEnableEnabled); err != nil {
		return fmt.Errorf("failed to enable USB: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	return nil
}

func findGpioChip() (*gpiocdev.Chip, error) {
	paths, err := filepath.Glob("/dev/gpiochip*")
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		c, err := gpiocdev.NewChip(p, gpiocdev.WithConsumer("usb-switch"))
		if err != nil {
			continue
		}
		if strings.HasPrefix(c.Label, gpioWantedPrefix) {
			return c, nil
		}
		c.Close()
	}
	return nil, fmt.Errorf("no GPIO chip found with prefix '%s'", gpioWantedPrefix)
}

// SwitchUSBMode changes the USB controller mode by swapping device tree overlays.
func SwitchUSBMode(mode USBMode) error {
	modeStr := mode.String()
	if modeStr == "unknown" {
		return fmt.Errorf("invalid USB mode")
	}

	configFS := "/sys/kernel/config/device-tree/overlays"
	dwc2Driver := "/sys/bus/platform/drivers/dwc2"
	usbID := "3f980000.usb"

	log.Printf("Switching USB to %s mode...", modeStr)

	// We ignore the error here as it might already be unbound.
	_ = os.WriteFile(filepath.Join(dwc2Driver, "unbind"), []byte(usbID), 0200)

	// Toggle the GPIO output after unbind and before bind.
	if err := setUSBSelect(mode); err != nil {
		log.Printf("Warning: failed to set USB select GPIO: %v", err)
	}

	// Remove all dwc2 overlays to ensure a clean state.
	_ = os.Remove(filepath.Join(configFS, "dwc2-host"))
	_ = os.Remove(filepath.Join(configFS, "dwc2-peripheral"))

	// Apply new overlay.
	overlayPath := filepath.Join(configFS, "dwc2-"+modeStr)
	if err := os.Mkdir(overlayPath, 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create overlay directory %s: %w", overlayPath, err)
	}

	dtboBytes, err := os.ReadFile(fmt.Sprintf("/etc/dwc2-%s.dtbo", modeStr))
	if err != nil {
		return fmt.Errorf("failed to read overlay file /etc/dwc2-%s.dtbo: %w", modeStr, err)
	}

	if err := os.WriteFile(filepath.Join(overlayPath, "dtbo"), dtboBytes, 0644); err != nil {
		return fmt.Errorf("failed to write dtbo to configfs: %w", err)
	}

	// Rebind: write the USB ID back to the bind file.
	if err := os.WriteFile(filepath.Join(dwc2Driver, "bind"), []byte(usbID), 0200); err != nil {
		return fmt.Errorf("failed to rebind dwc2: %w", err)
	}

	// Poll until the dr_mode transitions to the requested mode
	drModePath := fmt.Sprintf("/sys/kernel/debug/usb/%s/dr_mode", usbID)
	for i := 0; i < 50; i++ {
		data, err := os.ReadFile(drModePath)
		if err == nil {
			currentMode := strings.TrimSpace(string(data))
			if currentMode == modeStr {
				log.Printf("Done. Mode is now: %s", currentMode)
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	log.Printf("Warning: timed out waiting for dr_mode to become %s", modeStr)
	return nil
}
