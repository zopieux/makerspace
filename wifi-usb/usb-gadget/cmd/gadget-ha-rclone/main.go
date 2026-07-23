package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gauthbox"
	"usb-gadget/usb"
)

var (
	globalConfig = &gauthbox.AuthboxConfig{
		MqttBroker: &gauthbox.MqttConfig{
			Broker:     "tcp://localhost:1883",
			BaseTopic:  "onefinity_cnc",
			DeviceId:   "onefinity_cnc",
			DeviceName: "Onefinity CNC",
		},
		BadgeReader: &gauthbox.BadgeReaderConfig{
			Name:      "HID OMNIKEY",
			TimeoutMs: 250,
		},
		BadgeAuth: &gauthbox.BadgeAuthConfig{
			UrlTemplate:  "",
			UsageMinutes: 10,
		},
	}
	conf = gauthbox.UsbGadgetConfig{
		RclonePrefix:      "drive:Dirname",
		MaxTransfer:       "64M",
		MaxSize:           "10M",
		MaxAge:            "365d",
		HaSensorModel:     "Onefinity CNC USB Storage",
		HaSensorManuf:     "ZRH Woodshop",
		UsbLabel:          "WOODZRH",
		UsbSerialNumber:   "0123456789",
		UsbManufacturer:   "WOODZRH",
		UsbProduct:        "DropPortal",
		UsbConfigName:     "USB Storage",
		UsbInquiryVendor:  "WOODZRH",
		UsbInquiryProduct: "DropPortal",
		UsbSelectPin:      intPtr(0),
		UsbEnablePin:      intPtr(1),
		StatusLed:         nil,
		Button:            nil,
	}
)

const (
	portalDir           = "/dev/shm/usb-gadget"
	imgFile             = "/dev/shm/usb-gadget.img"
	retryInterval       = 2 * time.Second
	longPressDuration   = 3 * time.Second
	doublePressDuration = 500 * time.Millisecond
)

var (
	isAttaching     bool
	lastStatus      string
	currentUSBMode  usb.USBMode = usb.USBModeHost
	currentUsername string

	usbCtrl     usb.Orchestrator = &usb.RealOrchestrator{}
	mqttPublish gauthbox.PublishFunc
	cmdQueue    = make(chan func(), 10)

	statusComponent   gauthbox.MqttComponent
	usernameComponent gauthbox.MqttComponent
	detachComponent   gauthbox.MqttComponent

	wakeBadgeReader = make(chan struct{}, 1)
	statusLed       gauthbox.GpioLine
)

var configUrl = getEnv("CONFIG_URL", "http://example.org/config/onefinity-cnc")
var rcloneConfigPath = getEnv("RCLONE_CONFIG", "/root/.config/rclone/rclone.conf")

func wakeReader() {
	select {
	case wakeBadgeReader <- struct{}{}:
	default:
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

type FileSystem interface {
	RemoveAll(path string) error
	MkdirAll(path string, perm os.FileMode) error
	WriteFile(filename string, data []byte, perm os.FileMode) error
	ReadDir(dirname string) ([]os.DirEntry, error)
}

type RealFileSystem struct{}

func (r *RealFileSystem) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (r *RealFileSystem) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (r *RealFileSystem) WriteFile(filename string, data []byte, perm os.FileMode) error {
	return os.WriteFile(filename, data, perm)
}

func (r *RealFileSystem) ReadDir(dirname string) ([]os.DirEntry, error) {
	return os.ReadDir(dirname)
}

// For testing purposes.
var fsCtrl FileSystem = &RealFileSystem{}

type RcloneSyncer interface {
	Sync(rcloneConfigPath, source, portalDir string, extraConfig gauthbox.UsbGadgetConfig) ([]byte, error)
}

type RealRcloneSyncer struct{}

func (r *RealRcloneSyncer) Sync(rcloneConfigPath, source, portalDir string, extraConfig gauthbox.UsbGadgetConfig) ([]byte, error) {
	args := []string{"--config", rcloneConfigPath, "copy", source, portalDir}
	exts := strings.Join(usb.ValidExts, ",")
	args = append(args, "--include", fmt.Sprintf("*.{%s}", exts))
	args = append(args, "--include", fmt.Sprintf("**/*.{%s}", exts))
	args = append(args, "--inplace")
	if extraConfig.MaxAge != "" {
		args = append(args, "--max-age", extraConfig.MaxAge)
	}
	if extraConfig.MaxSize != "" {
		args = append(args, "--max-size", extraConfig.MaxSize)
	}
	if extraConfig.MaxTransfer != "" {
		args = append(args, "--max-transfer", extraConfig.MaxTransfer, "--cutoff-mode", "SOFT")
	}

	slog.Info("Running rclone", slog.Any("args", args))
	cmd := exec.Command("rclone", args...)
	return cmd.CombinedOutput()
}

var rcloneCtrl RcloneSyncer = &RealRcloneSyncer{}

func loadConfig() error {
	slog.Info("Fetching configuration", slog.String("url", configUrl))
	var ccBaseUrl, hostname string
	idx := strings.Index(configUrl, "/config/")
	if idx != -1 {
		ccBaseUrl = configUrl[:idx]
		hostname = configUrl[idx+len("/config/"):]
	} else {
		ccBaseUrl = configUrl
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			hostname = "onefinity-cnc"
		}
	}

	cfg, err := gauthbox.GetConfigForHostname(ccBaseUrl, hostname)
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}
	globalConfig = cfg

	if globalConfig.MqttBroker == nil {
		globalConfig.MqttBroker = &gauthbox.MqttConfig{
			DeviceId:   "onefinity_cnc",
			DeviceName: "Onefinity CNC",
		}
	} else {
		if globalConfig.MqttBroker.DeviceId == "" {
			globalConfig.MqttBroker.DeviceId = "onefinity_cnc"
		}
		if globalConfig.MqttBroker.DeviceName == "" {
			globalConfig.MqttBroker.DeviceName = "Onefinity CNC"
		}
	}

	if globalConfig.Gadget != nil {
		g := globalConfig.Gadget
		if g.RclonePrefix != "" {
			conf.RclonePrefix = g.RclonePrefix
		}
		if g.MaxTransfer != "" {
			conf.MaxTransfer = g.MaxTransfer
		}
		if g.MaxSize != "" {
			conf.MaxSize = g.MaxSize
		}
		if g.MaxAge != "" {
			conf.MaxAge = g.MaxAge
		}
		if g.HaSensorModel != "" {
			conf.HaSensorModel = g.HaSensorModel
		}
		if g.HaSensorManuf != "" {
			conf.HaSensorManuf = g.HaSensorManuf
		}
		if g.UsbLabel != "" {
			conf.UsbLabel = g.UsbLabel
		}
		if g.UsbSerialNumber != "" {
			conf.UsbSerialNumber = g.UsbSerialNumber
		}
		if g.UsbManufacturer != "" {
			conf.UsbManufacturer = g.UsbManufacturer
		}
		if g.UsbProduct != "" {
			conf.UsbProduct = g.UsbProduct
		}
		if g.UsbConfigName != "" {
			conf.UsbConfigName = g.UsbConfigName
		}
		if g.UsbInquiryVendor != "" {
			conf.UsbInquiryVendor = g.UsbInquiryVendor
		}
		if g.UsbInquiryProduct != "" {
			conf.UsbInquiryProduct = g.UsbInquiryProduct
		}
		if g.UsbSelectPin != nil {
			conf.UsbSelectPin = g.UsbSelectPin
		}
		if g.UsbEnablePin != nil {
			conf.UsbEnablePin = g.UsbEnablePin
		}
		if g.Button != nil {
			conf.Button = g.Button
		}
		if g.StatusLed != nil {
			conf.StatusLed = g.StatusLed
		}
	}

	return nil
}

func reportStatus() {
	state := "detached"
	if isAttaching {
		state = "attaching"
	} else if usbCtrl.IsBound() {
		state = "attached"
	}
	if statusLed != nil {
		on := (state == "attached")
		val := 0
		if on {
			val = 1
		}
		if conf.StatusLed.ActiveLow {
			val = 1 - val
		}
		statusLed.SetValue(val)
	}
	if state != lastStatus {
		if mqttPublish != nil {
			mqttPublish(statusComponent, state)
		}
		lastStatus = state
	}
}

func syncAndAttach(username string) (usb.UblkDevice, error) {
	isAttaching = true
	reportStatus()
	defer func() {
		isAttaching = false
		reportStatus()
	}()

	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("empty username")
	}

	slog.Info("Syncing for user", slog.String("username", username))
	source := fmt.Sprintf("%s/%s@", conf.RclonePrefix, username)

	// Clean slate
	fsCtrl.RemoveAll(portalDir)
	fsCtrl.MkdirAll(portalDir, 0755)

	var wg sync.WaitGroup
	wg.Add(2)
	usbErrChan := make(chan error, 1)

	// Copy files and prepare mount directory.
	go func() {
		defer wg.Done()
		addFile := func(msg, content string) {
			m := fmt.Sprintf("[ %s ].txt", msg)
			fsCtrl.WriteFile(filepath.Join(portalDir, m), []byte(content), 0644)
		}

		out, err := rcloneCtrl.Sync(rcloneConfigPath, source, portalDir, conf)
		if err != nil {
			slog.Error("rclone failed", slog.Any("error", err), slog.String("output", string(out)))
		}

		entries, _ := fsCtrl.ReadDir(portalDir)
		count := len(entries)
		slog.Info("Found files for user", slog.Int("count", count), slog.String("username", username))
		addFile(fmt.Sprintf("logged in as %s", username), "")
		if err != nil {
			addFile("rclone error - notify organizers", string(out))
		} else if count == 0 {
			addFile("no compatible files in your Drive", "Add some files, then log out and log in again.")
		}
	}()

	// Setup USB peripheral mode and ublk gadget
	go func() {
		defer wg.Done()
		if err := usbCtrl.SwitchUSBMode(usb.USBModePeripheral); err != nil {
			slog.Error("Failed to switch USB mode", slog.Any("error", err))
			usbErrChan <- err
			return
		}
		currentUSBMode = usb.USBModePeripheral

		cfg := usb.GadgetConfig{
			VendorID:        "0x1d6b",
			ProductID:       "0x0104",
			SerialNumber:    conf.UsbSerialNumber,
			Manufacturer:    conf.UsbManufacturer,
			Product:         conf.UsbProduct,
			ConfigName:      conf.UsbConfigName,
			MaxPower:        250,
			InquiryVendor:   conf.UsbInquiryVendor,
			InquiryProduct:  conf.UsbInquiryProduct,
			InquiryRevision: "1.0",
		}

		if err := usbCtrl.GadgetInit(cfg); err != nil {
			slog.Error("Failed to init gadget", slog.Any("error", err))
			usbErrChan <- err
			return
		}
	}()

	wg.Wait()
	close(usbErrChan)

	if err := <-usbErrChan; err != nil {
		slog.Error("USB error, cleaning up", slog.Any("error", err))
		fsCtrl.RemoveAll(portalDir)
		return nil, err
	}

	srv, err := usbCtrl.UblkUpdate(portalDir, conf.UsbLabel)
	if err != nil {
		return nil, err
	}
	return srv, nil
}

func doAttach(username string) {
	if srv, err := syncAndAttach(username); err == nil && srv != nil {
		currentUsername = username
		publishUsername(username)
	}
}

func doDetach() {
	if currentUSBMode == usb.USBModeHost {
		return
	}
	currentUSBMode = usb.USBModeHost
	usbCtrl.Detach()
	usbCtrl.GadgetClose()
	if err := usbCtrl.SwitchUSBMode(usb.USBModeHost); err != nil {
		slog.Error("Failed to switch USB mode to host", slog.Any("error", err))
	}
	currentUsername = ""
	publishUsername("")
	wakeReader()
}

func publishUsername(username string) {
	if mqttPublish != nil {
		mqttPublish(usernameComponent, username)
	}
}

func main() {
	if err := loadConfig(); err != nil {
		slog.Error("Failed to load config", slog.Any("error", err))
		os.Exit(1)
	}

	if conf.UsbSelectPin != nil {
		usb.UsbSelectPin = *conf.UsbSelectPin
	}
	if conf.UsbEnablePin != nil {
		usb.UsbEnablePin = *conf.UsbEnablePin
	}

	if conf.StatusLed != nil {
		var err error
		initVal := 0
		if conf.StatusLed.ActiveLow {
			initVal = 1
		}
		statusLed, err = gauthbox.RequestOutputPinFn(conf.StatusLed.Pin, initVal)
		if err != nil {
			slog.Error("Failed to initialize status LED", slog.Int("pin", conf.StatusLed.Pin), slog.Any("error", err))
		} else {
			slog.Info("Initialized status LED", slog.Int("pin", conf.StatusLed.Pin))
		}
	}

	usbCtrl.Detach()
	usbCtrl.GadgetClose()
	if err := usbCtrl.SwitchUSBMode(usb.USBModeHost); err != nil {
		slog.Error("Failed to switch USB mode to host at startup", slog.Any("error", err))
	}
	currentUSBMode = usb.USBModeHost

	if conf.Button != nil {
		btn := conf.Button
		var (
			mu                 sync.Mutex
			pressCount         int
			longPressTriggered bool
			longPressTimer     *time.Timer
			doublePressTimer   *time.Timer
		)

		isPressed := func(high bool) bool {
			if btn.ActiveLow {
				return !high
			}
			return high
		}

		_, err := gauthbox.RequestInputPinFn(btn.Pin, btn.Bias, time.Duration(btn.DebounceMs)*time.Millisecond, func(high bool) {
			mu.Lock()
			defer mu.Unlock()

			if isPressed(high) {
				longPressTriggered = false
				if longPressTimer != nil {
					longPressTimer.Stop()
				}
				longPressTimer = time.AfterFunc(longPressDuration, func() {
					mu.Lock()
					defer mu.Unlock()
					longPressTriggered = true
					pressCount = 0
					if doublePressTimer != nil {
						doublePressTimer.Stop()
						doublePressTimer = nil
					}
					cmdQueue <- func() {
						if currentUsername != "" {
							slog.Info("Button long pressed; logging out & detaching", slog.String("username", currentUsername))
							currentUsername = ""
							doDetach()
						}
					}
				})

				pressCount++
				if doublePressTimer != nil {
					doublePressTimer.Stop()
					doublePressTimer = nil
				}
			} else {
				if longPressTimer != nil {
					longPressTimer.Stop()
					longPressTimer = nil
				}

				if longPressTriggered {
					longPressTriggered = false
					pressCount = 0
					return
				}

				if pressCount == 1 {
					doublePressTimer = time.AfterFunc(doublePressDuration, func() {
						mu.Lock()
						defer mu.Unlock()
						pressCount = 0
						doublePressTimer = nil
					})
				} else if pressCount == 2 {
					pressCount = 0
					cmdQueue <- func() {
						if currentUsername != "" {
							username := currentUsername
							currentUsername = ""
							slog.Info("Button double pressed; reloading", slog.String("username", username))
							doDetach()
							doAttach(username)
						}
					}
				}
			}
		})
		if err != nil {
			slog.Error("Failed to initialize button", slog.Int("pin", btn.Pin), slog.Any("error", err))
		} else {
			slog.Info("Initialized button", slog.Int("pin", btn.Pin))
		}
	}

	// Define MQTT components
	statusComponent = gauthbox.MqttComponent{
		Id: "status",
		Component: func(baseTopic string) gauthbox.HaComponent {
			return gauthbox.HaComponent{
				Name:        "Storage status",
				Platform:    "sensor",
				BaseTopic:   baseTopic,
				StateTopic:  "~/state",
				DeviceClass: "enum",
				Options:     []string{"detached", "attaching", "attached"},
			}
		},
		Publish: func(payload interface{}) (string, interface{}) {
			return "/state", payload.(string)
		},
	}

	usernameComponent = gauthbox.MqttComponent{
		Id: "username",
		Component: func(baseTopic string) gauthbox.HaComponent {
			return gauthbox.HaComponent{
				Name:         "Username",
				Platform:     "text",
				BaseTopic:    baseTopic,
				StateTopic:   "~/state",
				CommandTopic: "~/cmd",
			}
		},
		Publish: func(payload interface{}) (string, interface{}) {
			return "/state", payload.(string)
		},
		Subscribe: []gauthbox.MqttSub{
			{
				Topic: "/cmd",
				Callback: func(topic string, payload string) {
					slog.Info("Requested attach for user", slog.String("username", payload))
					cmdQueue <- func() {
						doAttach(payload)
					}
				},
			},
		},
	}

	detachComponent = gauthbox.MqttComponent{
		Id: "detach",
		Component: func(baseTopic string) gauthbox.HaComponent {
			return gauthbox.HaComponent{
				Name:         "Detach storage",
				Platform:     "button",
				BaseTopic:    baseTopic,
				CommandTopic: "~/cmd",
			}
		},
		Subscribe: []gauthbox.MqttSub{
			{
				Topic: "/cmd",
				Callback: func(topic string, payload string) {
					slog.Info("Requested detach")
					cmdQueue <- func() {
						doDetach()
					}
				},
			},
		},
	}

	// MQTT.
	components := []gauthbox.MqttComponent{statusComponent, usernameComponent, detachComponent}
	looper, events, publish := gauthbox.MqttBroker(*globalConfig.MqttBroker, components)
	mqttPublish = publish

	go looper()

	// Handle connection events
	go func() {
		for e := range events {
			switch ev := e.(type) {
			case gauthbox.MqttConnected:
				slog.Info("Connected to MQTT broker")
				lastStatus = "" // Force re-report
				reportStatus()
			case gauthbox.MqttDisonnected:
				slog.Error("Disconnected from MQTT broker", slog.Any("error", ev.Error))
			case gauthbox.MqttResetRequest:
				slog.Info("Requested reset from MQTT")
				cmdQueue <- func() {
					doDetach()
				}
			}
		}
	}()

	// Worker queue
	go func() {
		for f := range cmdQueue {
			f()
			reportStatus()
		}
	}()

	// Badge reader loop
	go func() {
		for {
			if currentUSBMode != usb.USBModeHost {
				select {
				case <-wakeBadgeReader:
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}

			if globalConfig.BadgeReader == nil || globalConfig.BadgeAuth == nil {
				slog.Error("missing badge_reader or badge_auth config")
				select {
				case <-wakeBadgeReader:
				case <-time.After(retryInterval):
				}
				continue
			}

			slog.Info("Starting badge reader", slog.String("name", globalConfig.BadgeReader.Name))
			badgeDev, err := gauthbox.BadgeReaderFn(*globalConfig.BadgeReader)
			if err != nil {
				slog.Error("Failed to initialize badge reader", slog.Any("error", err))
				select {
				case <-wakeBadgeReader:
				case <-time.After(retryInterval):
				}
				continue
			}

			slog.Info("Badge reader initialized; listening")
			go badgeDev.Looper()

			for badgeId := range badgeDev.Events {
				badgeId = strings.TrimSpace(badgeId)
				if badgeId == "" {
					continue
				}
				slog.Info("Received badge ID", slog.String("badgeId", badgeId))

				username, err := gauthbox.BadgeAuth(*globalConfig.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_INITIAL)
				if err != nil {
					slog.Error("Authentication failed for badge", slog.String("badgeId", badgeId), slog.Any("error", err))
					continue
				}

				if username == "" {
					slog.Error("Authentication failed for badge: no username in response", slog.String("badgeId", badgeId))
					continue
				}

				slog.Info("Authentication successful for badge", slog.String("badgeId", badgeId), slog.String("username", username))
				cmdQueue <- func() {
					doAttach(username)
				}
			}
			slog.Warn("Badge reader channel closed. Waiting to restart...")
			select {
			case <-wakeBadgeReader:
			case <-time.After(retryInterval):
			}
		}
	}()

	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		reportStatus()
	}
}

func intPtr(v int) *int {
	return &v
}
