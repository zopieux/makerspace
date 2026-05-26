package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"usb-gadget/usb"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"golang.org/x/net/context"
)

type Config struct {
	AuthUrlTemplate   string `json:"auth_url_template"`
	MqttBroker        string `json:"mqtt_broker"`
	MqttClientId      string `json:"mqtt_client_id"`
	MqttTopic         string `json:"mqtt_base_topic"`
	MqttDeviceId      string `json:"mqtt_device_id"`
	MqttDeviceName    string `json:"mqtt_device_name"`
	RclonePrefix      string `json:"rclone_prefix"`
	MaxTransfer       string `json:"max_transfer"`
	MaxSize           string `json:"max_size"`
	MaxAge            string `json:"max_age"`
	HaSensorModel     string `json:"ha_sensor_model"`
	HaSensorManuf     string `json:"ha_sensor_manufacturer"`
	UsbLabel          string `json:"usb_label"`
	UsbSerialNumber   string `json:"usb_serial_number"`
	UsbManufacturer   string `json:"usb_manufacturer"`
	UsbProduct        string `json:"usb_product"`
	UsbConfigName     string `json:"usb_config_name"`
	UsbInquiryVendor  string `json:"usb_inquiry_vendor"`
	UsbInquiryProduct string `json:"usb_inquiry_product"`
}

var config = Config{
	MqttBroker:        "tcp://localhost:1883",
	MqttClientId:      "onefinity_cnc",
	MqttTopic:         "onefinity_cnc",
	MqttDeviceId:      "onefinity_cnc",
	MqttDeviceName:    "Onefinity CNC",
	RclonePrefix:      "drive:Dirname",
	AuthUrlTemplate:   "",
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
}

const (
	portalDir     = "/dev/shm/usb-gadget"
	imgFile       = "/dev/shm/usb-gadget.img"
	retryInterval = 2 * time.Second
	logOutName    = "LOG OUT"
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

var configUrl = getEnv("CONFIG_URL", "http://example.org/config/onefinity-cnc")
var rcloneConfigPath = getEnv("RCLONE_CONFIG", "/root/.config/rclone/rclone.conf")

func loadConfig() error {
	log.Printf("Fetching configuration from %s", configUrl)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, configUrl, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("non-OK response fetching config: %d, '%s'", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return err
	}
	return nil
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
	Options             []string `json:"options,omitempty"`
	Device              Device   `json:"device"`
}

func publishDiscovery(client mqtt.Client) {
	device := Device{
		Identifiers:  []string{config.MqttDeviceId},
		Name:         config.MqttDeviceName,
		Model:        config.HaSensorModel,
		Manufacturer: config.HaSensorManuf,
	}
	availabilityTopic := config.MqttTopic + "/state/availability"

	// Sensor: Status
	statusDisc := DiscoveryMsg{
		Name:              "Storage status",
		StateTopic:        config.MqttTopic + "/state/status",
		AvailabilityTopic: availabilityTopic,
		UniqueID:          config.MqttDeviceId + "_status",
		DeviceClass:       "enum",
		Options:           []string{"detached", "attaching", "attached"},
		Device:            device,
	}
	sendDiscovery(client, "sensor", "status", statusDisc)

	// Text: Username
	userDisc := DiscoveryMsg{
		Name:              "Username",
		StateTopic:        config.MqttTopic + "/state/attach_username",
		CommandTopic:      config.MqttTopic + "/cmd/attach",
		AvailabilityTopic: availabilityTopic,
		UniqueID:          config.MqttDeviceId + "_user",
		Device:            device,
	}
	sendDiscovery(client, "text", "username", userDisc)

	// Button: Detach
	detachDisc := DiscoveryMsg{
		Name:              "Detach storage",
		CommandTopic:      config.MqttTopic + "/cmd/detach",
		AvailabilityTopic: availabilityTopic,
		UniqueID:          config.MqttDeviceId + "_detach",
		Device:            device,
	}
	sendDiscovery(client, "button", "detach", detachDisc)
}

func sendDiscovery(client mqtt.Client, component, node string, msg interface{}) {
	topic := fmt.Sprintf("homeassistant/%s/%s/%s/config", component, config.MqttDeviceId, node)
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
		client.Publish(config.MqttTopic+"/state/status", 0, true, state)
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
	source := fmt.Sprintf("%s/%s@", config.RclonePrefix, username)

	// Clean slate
	os.RemoveAll(portalDir)
	os.MkdirAll(portalDir, 0755)

	var wg sync.WaitGroup
	wg.Add(2)
	usbErrChan := make(chan error, 1)

	// Copy files and prepare mount directory.
	go func() {
		defer wg.Done()
		args := []string{"--config", rcloneConfigPath, "copy", source, portalDir}
		exts := strings.Join(usb.ValidExts, ",")
		args = append(args, "--include", fmt.Sprintf("*.{%s}", exts))
		args = append(args, "--include", fmt.Sprintf("**/*.{%s}", exts))
		args = append(args, "--inplace")
		if config.MaxAge != "" {
			args = append(args, "--max-age", config.MaxAge)
		}
		if config.MaxSize != "" {
			args = append(args, "--max-size", config.MaxSize)
		}
		if config.MaxTransfer != "" {
			args = append(args, "--max-transfer", config.MaxTransfer, "--cutoff-mode", "SOFT")
		}

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
		addFile(logOutName, "Please wait...")
	}()

	// Setup USB peripheral mode and ublk gadget
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
			SerialNumber:    config.UsbSerialNumber,
			Manufacturer:    config.UsbManufacturer,
			Product:         config.UsbProduct,
			ConfigName:      config.UsbConfigName,
			MaxPower:        250,
			InquiryVendor:   config.UsbInquiryVendor,
			InquiryProduct:  config.UsbInquiryProduct,
			InquiryRevision: "1.0",
		}

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

	srv := usb.UblkUpdate(portalDir, config.UsbLabel)
	return srv, nil
}

func doAttach(client mqtt.Client, username string, cmdQueue chan<- func()) {
	logOutFilePath := fmt.Sprintf("[ %s ].txt", logOutName)
	if srv, err := syncAndAttach(client, username); err == nil && srv != nil {
		publishUsername(client, username)
		go func() {
			for {
				select {
				case <-srv.Closed():
					return
				case relPath := <-srv.DeleteEvents:
					if relPath == logOutFilePath {
						log.Printf("User logged out via LOG OUT file deletion")
						cmdQueue <- func() {
							doDetach(client)
						}
					}
				case fileName := <-srv.ReadEvents:
					if fileName == logOutFilePath {
						log.Printf("User logged out via LOG OUT file read")
						cmdQueue <- func() {
							doDetach(client)
						}
					}
				}
			}
		}()
	}
}

func doDetach(client mqtt.Client) {
	if currentUSBMode == usb.USBModeHost {
		return
	}
	currentUSBMode = usb.USBModeHost
	usb.Detach()
	usb.GadgetClose()
	if err := usb.SwitchUSBMode(usb.USBModeHost); err != nil {
		log.Printf("Failed to switch USB mode to host: %v", err)
	}
	publishUsername(client, "")
}

func publishUsername(client mqtt.Client, username string) {
	client.Publish(config.MqttTopic+"/state/attach_username", 0, true, username)
}

func doBadgeAuth(badgeId string, urlTemplate string, state string) (string, error) {
	t, err := template.New("url").Parse(urlTemplate)
	if err != nil {
		return "", err
	}
	var url strings.Builder
	err = t.Execute(&url, map[string]interface{}{
		"badgeId":  badgeId,
		"state":    state,
		"duration": 10,
	})
	if err != nil {
		return "", err
	}
	resp, err := http.Post(url.String(), "text/plain", strings.NewReader(""))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var reason []byte
		if reason, err = io.ReadAll(io.LimitReader(resp.Body, 256)); err != nil {
			reason = []byte("(can't decode body)")
		}
		return "", errors.New("error authenticating badge: " + string(reason))
	}
	username := resp.Header.Get("x-makerspace-username")
	if username == "" {
		return "", errors.New("no username in response")
	}
	return username, nil
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	usb.Detach()
	usb.GadgetClose()
	if err := usb.SwitchUSBMode(usb.USBModeHost); err != nil {
		log.Printf("Failed to switch USB mode to host at startup: %v", err)
	}
	currentUSBMode = usb.USBModeHost

	availabilityTopic := config.MqttTopic + "/state/availability"
	opts := mqtt.NewClientOptions().AddBroker(config.MqttBroker).SetClientID(config.MqttClientId).SetAutoReconnect(true).SetWill(availabilityTopic, "offline", 0, true)

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

				username, err := doBadgeAuth(badgeId, config.AuthUrlTemplate, "")
				if err != nil {
					log.Printf("Authentication failed for badge %s: %v", badgeId, err)
					continue
				}

				log.Printf("Authentication successful for badge %s (%s). Attaching...", badgeId, username)
				cmdQueue <- func() {
					if client != nil {
						doAttach(client, username, cmdQueue)
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
		client.Subscribe(config.MqttTopic+"/cmd/attach", 0, func(c mqtt.Client, m mqtt.Message) {
			username := string(m.Payload())
			log.Printf("Requested attach for user: %s", username)
			cmdQueue <- func() {
				doAttach(c, username, cmdQueue)
			}
		})
		client.Subscribe(config.MqttTopic+"/cmd/detach", 0, func(c mqtt.Client, m mqtt.Message) {
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
