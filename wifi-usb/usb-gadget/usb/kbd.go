package usb

import (
	"fmt"
	"strings"

	evdev "github.com/gvalkov/golang-evdev"
)

const MANUFACTURER = "HID OMNIKEY"

// FindReader looks for a keyboard device with a specific manufacturer string.
func FindReader() (string, error) {
	devices, err := evdev.ListInputDevices()
	if err != nil {
		return "", fmt.Errorf("failed to list input devices: %w", err)
	}

	for _, dev := range devices {
		if strings.Contains(dev.Name, MANUFACTURER) {
			return dev.Fn, nil
		}
	}

	return "", fmt.Errorf("%s reader not found", MANUFACTURER)
}

var shiftMap = map[uint16]string{
	uint16(evdev.KEY_1): "!", uint16(evdev.KEY_2): "@", uint16(evdev.KEY_3): "#",
	uint16(evdev.KEY_4): "$", uint16(evdev.KEY_5): "%", uint16(evdev.KEY_6): "^",
	uint16(evdev.KEY_7): "&", uint16(evdev.KEY_8): "*", uint16(evdev.KEY_9): "(",
	uint16(evdev.KEY_0): ")", uint16(evdev.KEY_MINUS): "_", uint16(evdev.KEY_EQUAL): "+",
	uint16(evdev.KEY_SPACE): " ",
}

var normalMap = map[uint16]string{
	uint16(evdev.KEY_1): "1", uint16(evdev.KEY_2): "2", uint16(evdev.KEY_3): "3",
	uint16(evdev.KEY_4): "4", uint16(evdev.KEY_5): "5", uint16(evdev.KEY_6): "6",
	uint16(evdev.KEY_7): "7", uint16(evdev.KEY_8): "8", uint16(evdev.KEY_9): "9",
	uint16(evdev.KEY_0): "0", uint16(evdev.KEY_MINUS): "-", uint16(evdev.KEY_EQUAL): "=",
	uint16(evdev.KEY_ENTER): "\n",
	uint16(evdev.KEY_SPACE): " ",
}

// ReadKeyboardLines opens the specified evdev device and returns a channel
// that emits full lines of text (ending with Enter).
func ReadKeyboardLines(devPath string) (<-chan string, error) {
	dev, err := evdev.Open(devPath)
	if err != nil {
		return nil, err
	}

	out := make(chan string)

	go func() {
		defer close(out)
		var line strings.Builder
		shifted := false

		for {
			events, err := dev.Read()
			if err != nil {
				return
			}
			for _, e := range events {
				if e.Type != evdev.EV_KEY {
					continue
				}
				code := e.Code

				// Track shift state
				if code == uint16(evdev.KEY_LEFTSHIFT) || code == uint16(evdev.KEY_RIGHTSHIFT) {
					shifted = e.Value == 1 || e.Value == 2
					continue
				}

				// Only handle key down (1) or repeat (2)
				// The original snippet only handled e.Value == 1 for characters.
				// I'll stick to e.Value == 1 for characters to match the snippet.
				if e.Value != 1 {
					continue
				}

				var ch string
				if shifted {
					if s, ok := shiftMap[code]; ok {
						ch = s
					} else {
						name := evdev.KEY[int(code)] // e.g. "KEY_A"
						if len(name) == 5 {     // KEY_A .. KEY_Z
							ch = string(name[4]) // already upper
						}
					}
				} else {
					if s, ok := normalMap[code]; ok {
						ch = s
					} else {
						name := evdev.KEY[int(code)]
						if len(name) == 5 {
							ch = string(name[4] + 32) // to lower
						}
					}
				}

				if ch == "\n" {
					out <- line.String()
					line.Reset()
				} else if ch != "" {
					line.WriteString(ch)
				} else if code == uint16(evdev.KEY_BACKSPACE) {
					// Added basic backspace support since it's common
					s := line.String()
					if len(s) > 0 {
						line.Reset()
						line.WriteString(s[:len(s)-1])
					}
				}
			}
		}
	}()

	return out, nil
}
