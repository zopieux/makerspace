package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"usb-gadget/usb_gadget"

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
	portalDir = "/dev/shm/usb-gadget"
	imgFile   = "/dev/shm/usb-gadget.img"
)

var (
	isAttaching bool
	lastStatus  string
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
	} else if usb_gadget.IsBound() {
		state = "attached"
	}
	if state != lastStatus {
		client.Publish(baseTopic+"/state/status", 0, true, state)
		lastStatus = state
	}
}

func syncAndAttach(client mqtt.Client, username string) {
	isAttaching = true
	reportStatus(client)
	defer func() {
		isAttaching = false
		reportStatus(client)
	}()

	username = strings.TrimSpace(username)
	if username == "" {
		return
	}

	log.Printf("Syncing for user: %s", username)
	source := fmt.Sprintf("%s/%s@", rcloneRemote, username)
	
	// Clean slate
	os.RemoveAll(portalDir)
	os.MkdirAll(portalDir, 0755)
	
	// Prepare rclone include flags
	args := []string{"copy", source, portalDir}
	// Prepare rclone include flags using brace expansion
	exts := strings.Join(usb_gadget.ValidExts, ",")
	args = append(args, "--include", fmt.Sprintf("**/*{%s}", exts))
	args = append(args, "--max-age", "365d", "--max-size", "10M", "--inplace")

	logAsFile := func(msg string) {
		m := fmt.Sprintf("[ %s ].txt", msg)
		os.WriteFile(filepath.Join(portalDir, m), []byte(""), 0644)
	}

	log.Printf("Running rclone %v", args)
	cmd := exec.Command("rclone", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("rclone failed: %v, out: %s", err, string(out))
		logAsFile("rclone error - notify organizers")
	}

	entries, _ := os.ReadDir(portalDir)
	if len(entries) == 0 {
		logAsFile("No compatible files in your drive")
	}

	usb_gadget.Update(portalDir, imgFile)
}

func main() {
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
				syncAndAttach(c, username)
				c.Publish(baseTopic+"/state/attach_username", 0, true, username)
			}
		})
		client.Subscribe(baseTopic+"/cmd/detach", 0, func(c mqtt.Client, m mqtt.Message) {
			log.Println("Requested detach")
			cmdQueue <- func() {
				usb_gadget.Detach()
			}
		})
	})

	client = mqtt.NewClient(opts)
	for {
		if token := client.Connect(); token.Wait() && token.Error() != nil {
			log.Printf("Failed to connect to MQTT: %v, retrying in 5s...", token.Error())
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}

	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		reportStatus(client)
	}
}
