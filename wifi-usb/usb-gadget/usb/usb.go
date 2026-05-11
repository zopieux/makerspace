package usb

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/warthog618/go-gpiocdev"
)

const (
	usbSelectPin     = 27
	gpioWantedPrefix = "pinctrl-bcm2"
)

type USBMode int

const (
	USBModeHost USBMode = iota
	USBModePeripheral
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

// setUSBSelect toggles the GPIO pin to switch the TS3USB221 USB selector.
func setUSBSelect(mode USBMode) error {
	chip, err := findGpioChip()
	if err != nil {
		return err
	}
	defer chip.Close()

	val := map[USBMode]int{
		USBModeHost:       0,
		USBModePeripheral: 1,
	}[mode]
	line, err := chip.RequestLine(usbSelectPin, gpiocdev.AsOutput(val))
	if err != nil {
		return fmt.Errorf("failed to request GPIO line %d: %w", usbSelectPin, err)
	}
	line.Close()

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

	// Log the final mode from debugfs if available.
	if data, err := os.ReadFile(fmt.Sprintf("/sys/kernel/debug/usb/%s/dr_mode", usbID)); err == nil {
		log.Printf("Done. Mode is now: %s", strings.TrimSpace(string(data)))
	}

	return nil
}
