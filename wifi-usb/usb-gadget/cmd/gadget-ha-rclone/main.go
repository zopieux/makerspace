package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"usb-gadget/usb"

	// "gauthbox"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var (
	broker       = getEnv("MQTT_BROKER", "tcp://localhost:1883")
	clientID     = getEnv("MQTT_CLIENT_ID", "ha-rclone-gadget")
	baseTopic    = getEnv("MQTT_BASE_TOPIC", "usb-gadget")
	rcloneRemote = getEnv("RCLONE_PREFIX", "drive:usb-gadget")
	deviceID     = getEnv("DEVICE_ID", "usb_gadget_portal")
)

const (
	portalDir     = "/dev/shm/usb-gadget"
	imgFile       = "/dev/shm/usb-gadget.img"
	retryInterval = 2 * time.Second
)

var (
	isAttaching    bool
	lastStatus     string
	currentUSBMode usb.USBMode = usb.USBModeHost
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

type Device struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	Manufacturer string   `json:"manufacturer"`
}

type DiscoveryMsg struct {
	Name                string   `json:"name,omitempty"`
	StateTopic          string   `json:"state_topic,omitempty"`
	CommandTopic        string   `json:"command_topic,omitempty"`
	AvailabilityTopic   string   `json:"availability_topic,omitempty"`
	PayloadAvailable    string   `json:"payload_available,omitempty"`
	PayloadNotAvailable string   `json:"payload_not_available,omitempty"`
	PayloadOn           string   `json:"payload_on,omitempty"`
	PayloadOff          string   `json:"payload_off,omitempty"`
	DeviceClass         string   `json:"device_class,omitempty"`
	UniqueID            string   `json:"unique_id"`
	DefaultEntityID     string   `json:"default_entity_id,omitempty"`
	Options             []string `json:"options,omitempty"`
	Device              Device   `json:"device"`
}

func publishDiscovery(client mqtt.Client) {
	device := Device{
		Identifiers:  []string{deviceID},
		Name:         "MASSO USB Storage",
		Model:        "Pi Zero 2",
		Manufacturer: "ZRH Woodshop",
	}
	availabilityTopic := baseTopic + "/state/availability"

	// Sensor: Status
	statusDisc := DiscoveryMsg{
		Name:              "Gadget Status",
		StateTopic:        baseTopic + "/state/status",
		AvailabilityTopic: availabilityTopic,
		UniqueID:          deviceID + "_status",
		DeviceClass:       "enum",
		Options:           []string{"detached", "attaching", "attached"},
		Device:            device,
	}
	sendDiscovery(client, "sensor", "status", statusDisc)

	// Text: Username
	userDisc := DiscoveryMsg{
		Name:              "Attach Username",
		StateTopic:        baseTopic + "/state/attach_username",
		CommandTopic:      baseTopic + "/cmd/attach",
		AvailabilityTopic: availabilityTopic,
		UniqueID:          deviceID + "_user",
		DefaultEntityID:   "text.usb_gadget_username",
		Device:            device,
	}
	sendDiscovery(client, "text", "username", userDisc)

	// Button: Detach
	detachDisc := DiscoveryMsg{
		Name:              "Detach Gadget",
		CommandTopic:      baseTopic + "/cmd/detach",
		AvailabilityTopic: availabilityTopic,
		UniqueID:          deviceID + "_detach",
		DefaultEntityID:   "button.usb_gadget_detach",
		Device:            device,
	}
	sendDiscovery(client, "button", "detach", detachDisc)
}

func sendDiscovery(client mqtt.Client, component, node string, msg interface{}) {
	topic := fmt.Sprintf("homeassistant/%s/%s/%s/config", component, deviceID, node)
	payload, _ := json.Marshal(msg)
	client.Publish(topic, 0, true, payload)
}

func reportStatus(client mqtt.Client) {
	if client == nil || !client.IsConnected() {
		return
	}
	state := "detached"
	if isAttaching {
		state = "attaching"
	} else if usb.IsBound() {
		state = "attached"
	}
	if state != lastStatus {
		client.Publish(baseTopic+"/state/status", 0, true, state)
		lastStatus = state
	}
}

func syncAndAttach(client mqtt.Client, username string) (*usb.UblkServer, error) {
	isAttaching = true
	reportStatus(client)
	defer func() {
		isAttaching = false
		reportStatus(client)
	}()

	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("empty username")
	}

	log.Printf("Syncing for user: %s", username)
	source := fmt.Sprintf("%s/%s@", rcloneRemote, username)

	// Clean slate
	os.RemoveAll(portalDir)
	os.MkdirAll(portalDir, 0755)

	var wg sync.WaitGroup
	wg.Add(2)
	usbErrChan := make(chan error, 1)

	// Goroutine 1: rclone
	go func() {
		defer wg.Done()
		// Prepare rclone include flags
		args := []string{"copy", source, portalDir}
		exts := strings.Join(usb.ValidExts, ",")
		args = append(args, "--include", fmt.Sprintf("*.{%s}", exts))
		args = append(args, "--include", fmt.Sprintf("**/*.{%s}", exts))
		args = append(args, "--max-age", "365d", "--max-size", "10M", "--inplace")

		addFile := func(msg, content string) {
			m := fmt.Sprintf("[ %s ].txt", msg)
			os.WriteFile(filepath.Join(portalDir, m), []byte(content), 0644)
		}

		log.Printf("Running rclone %v", args)
		cmd := exec.Command("rclone", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("rclone failed: %v, out: %s", err, out)
		}

		entries, _ := os.ReadDir(portalDir)
		count := len(entries)
		log.Printf("Found %d files for user %s", count, username)
		if err != nil {
			addFile("rclone error - notify organizers", string(out))
		} else if count == 0 {
			addFile("no compatible files in your drive", "")
		}
		addFile("LOG OUT", "")
	}()

	// Goroutine 2: USB Switch + GadgetInit
	go func() {
		defer wg.Done()
		if err := usb.SwitchUSBMode(usb.USBModePeripheral); err != nil {
			log.Printf("Failed to switch USB mode: %v", err)
			usbErrChan <- err
			return
		}
		currentUSBMode = usb.USBModePeripheral

		cfg := usb.GadgetConfig{
			VendorID:        "0x1d6b",
			ProductID:       "0x0104",
			SerialNumber:    "0123456789",
			Manufacturer:    "WOODZRH",
			Product:         "DropPortal",
			ConfigName:      "USB Storage",
			MaxPower:        250,
			InquiryVendor:   "WOODZRH",
			InquiryProduct:  "DropPortal",
			InquiryRevision: "1.0",
		}

		time.Sleep(500 * time.Millisecond)
		if err := usb.GadgetInit(cfg); err != nil {
			log.Printf("Failed to init gadget: %v", err)
			usbErrChan <- err
			return
		}
	}()

	wg.Wait()
	close(usbErrChan)

	if err := <-usbErrChan; err != nil {
		log.Printf("USB error, cleaning up: %v", err)
		os.RemoveAll(portalDir)
		return nil, err
	}

	srv := usb.UblkUpdate(portalDir, "DROPPORTAL")
	return srv, nil
}

func doAttach(client mqtt.Client, username string, cmdQueue chan<- func()) {
	if srv, err := syncAndAttach(client, username); err == nil {
		publishUsername(client, username)
		if srv != nil {
			go func() {
				for {
					select {
					case <-srv.Closed():
						return
					case relPath := <-srv.DeleteEvents:
						if relPath == "[ LOG OUT ].txt" {
							log.Printf("User logged out via gadget file deletion")
							cmdQueue <- func() {
								doDetach(client)
							}
						}
					}
				}
			}()
		}
	}
}

func doDetach(client mqtt.Client) {
	usb.Detach()
	usb.GadgetClose()
	if err := usb.SwitchUSBMode(usb.USBModeHost); err != nil {
		log.Printf("Failed to switch USB mode to host: %v", err)
	}
	currentUSBMode = usb.USBModeHost
	publishUsername(client, "")
}

func publishUsername(client mqtt.Client, username string) {
	client.Publish(baseTopic+"/state/attach_username", 0, true, username)
}

func main() {
	usb.Detach()
	usb.GadgetClose()
	if err := usb.SwitchUSBMode(usb.USBModeHost); err != nil {
		log.Printf("Failed to switch USB mode to host at startup: %v", err)
	}
	currentUSBMode = usb.USBModeHost

	availabilityTopic := baseTopic + "/state/availability"
	opts := mqtt.NewClientOptions().AddBroker(broker).SetClientID(clientID).SetAutoReconnect(true).SetWill(availabilityTopic, "offline", 0, true)

	// We need these vars available for the connect handler and the worker
	var client mqtt.Client
	cmdQueue := make(chan func(), 10)

	go func() {
		for f := range cmdQueue {
			f()
			if client != nil {
				reportStatus(client)
			}
		}
	}()

	go func() {
		for {
			var devPath string
			var err error
			for {
				devPath, err = usb.FindReader()
				if err == nil {
					break
				}
				if currentUSBMode == usb.USBModeHost {
					log.Printf("Waiting for badge reader: %v", err)
				}
				time.Sleep(retryInterval)
			}

			log.Printf("Badge reader found at: %s; listening", devPath)
			lines, err := usb.ReadKeyboardLines(devPath)
			if err != nil {
				log.Printf("Failed to read keyboard lines: %v", err)
				time.Sleep(retryInterval)
				continue
			}

			for badgeId := range lines {
				badgeId = strings.TrimSpace(badgeId)
				if badgeId == "" {
					continue
				}
				log.Printf("Received badge ID: %s", badgeId)

				// var cfg gauthbox.AuthboxConfig
				// cfg.BadgeAuth.UrlTemplate = getEnv("AUTH_URL", "http://authbox.zrh/auth?badge={{.badgeId}}")
				// cfg.BadgeAuth.UsageMinutes = 60
				// err := gauthbox.BadgeAuth(cfg.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_INITIAL)
				// if err != nil {
				// 	log.Printf("Authentication failed for badge %s: %v", badgeId, err)
				// 	continue
				// }
				badgeId = "zopi"

				log.Printf("Authentication successful for badge %s. Proceeding with attachment.", badgeId)
				cmdQueue <- func() {
					if client != nil {
						doAttach(client, badgeId, cmdQueue)
					}
				}
			}
			log.Println("Badge reader channel closed. Waiting to re-find...")
		}
	}()

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		client = c
		log.Println("Connected to MQTT broker")
		publishDiscovery(client)
		c.Publish(availabilityTopic, 0, true, "online")
		lastStatus = "" // Force re-report
		reportStatus(c)
		client.Subscribe(baseTopic+"/cmd/attach", 0, func(c mqtt.Client, m mqtt.Message) {
			username := string(m.Payload())
			log.Printf("Requested attach for user: %s", username)
			cmdQueue <- func() {
				doAttach(c, username, cmdQueue)
			}
		})
		client.Subscribe(baseTopic+"/cmd/detach", 0, func(c mqtt.Client, m mqtt.Message) {
			log.Println("Requested detach")
			cmdQueue <- func() {
				doDetach(c)
			}
		})
	})

	client = mqtt.NewClient(opts)
	for {
		if token := client.Connect(); token.Wait() && token.Error() != nil {
			log.Printf("Failed to connect to MQTT: %v, retrying in %v...", token.Error(), retryInterval)
			time.Sleep(retryInterval)
			continue
		}
		break
	}

	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		reportStatus(client)
	}
}
