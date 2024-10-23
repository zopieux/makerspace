package gauthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/holoplot/go-evdev"
	"github.com/warthog618/go-gpiocdev"
)

const BADGE_WANTED_VENDOR = 121
const BADGE_WANTED_PRODUCT = 6
const BADGE_TIMEOUT = 250 * time.Millisecond

const GPIO_WANTED_PREFIX = "pinctrl-bcm2"
const GPIO_DEBOUNCE = 100 * time.Millisecond

// const COMMAND_CONTROL_HOSTNAME = "control.shop"
// const COMMAND_CONTROL_URL = "http://" + COMMAND_CONTROL_HOSTNAME + ":8000"

const MQTT_TOPIC_PREFIX = "shop/"
const HA_TOPIC_PREFIX = "homeassistant/"

const BADGE_ACTION_INITIAL = "initial"
const BADGE_ACTION_EXTEND = "extend"
const BADGE_ACTION_RETURN = "return"

type badgeReaderConfig struct {
	Vendor    uint16 `json:"vendor,omitempty"`
	Product   uint16 `json:"product,omitempty"`
	Name      string `json:"name,omitempty"`
	TimeoutMs uint32 `json:"timeout_ms"`
}

type badgeAuthConfig struct {
	// .badge, .state, .duration
	UrlTemplate  string `json:"url_template"`
	UsageMinutes uint32 `json:"usage_duration_minutes"`
}

type relayConfig struct {
	Pin      int `json:"pin"`
	Debounce int `json:"debounce_ms"`
}

type currentSensingConfig struct {
	Pin        int `json:"pin"`
	DebounceMs int `json:"debounce_ms"`
}

type mqttConfig struct {
	Broker string `json:"broker"`
	Topic  string `json:"topic"`
}

type ledConfig struct {
	Pin int `json:"pin"`
}

type LedStatic struct {
	On bool
}
type LedModeBlink struct {
	Interval time.Duration
}

type AuthboxConfig struct {
	MqttBroker     *mqttConfig          `json:"mqtt,omitempty"`
	BadgeReader    badgeReaderConfig    `json:"badge_reader"`
	BadgeAuth      badgeAuthConfig      `json:"badge_auth"`
	CurrentSensing currentSensingConfig `json:"current_sensing"`
	Relay          relayConfig          `json:"relay"`
	GreenLed       ledConfig            `json:"green_led"`
	RedLed         ledConfig            `json:"red_led"`
	IdleSeconds    uint32               `json:"idle_duration_s"`
}

type BadgingChan = <-chan string
type CurrentSensingChan = <-chan bool
type RelayIsOnChan = <-chan bool

type MqttAvailability struct {
	PayloadAvailable    string `json:"payload_available,omitempty"`
	PayloadNotAvailable string `json:"payload_not_available,omitempty"`
	Topic               string `json:"topic"`
	ValueTemplate       string `json:"value_template,omitempty"`
}

type MqttDiscoveryAnnounceFunc func(name string, topic string) interface{}

type MqttDiscovery struct {
	// UniqueId     string             `json:"unique_id"`
	// StateTopic   string             `json:"state_topic"`
	// CommandTopic string             `json:"command_topic,omitempty"`
	// Availability []MqttAvailability `json:"availability,omitempty"`
	// PayloadOn    string             `json:"payload_on,omitempty"`
	// PayloadOff   string             `json:"payload_off,omitempty"`
	// Device       MqttDevice         `json:"device"`
	Component string
	Id        string
	Announce  MqttDiscoveryAnnounceFunc
}
type MqttDevice struct {
	Name         string `json:"string,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
}

type PublishFunc = func(topic string, payload interface{})
type DeviceRet[Event any] struct {
	Looper    func()
	Events    chan Event
	OnEvent   func(payload Event, name string, publish PublishFunc)
	Discovery MqttDiscovery
}

// Retrieves the config from command & control, falling back to the SD card if
// that fails.
func GetConfig(ccUrl string) (*AuthboxConfig, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	if config, err := getConfigRemotely(hostname, ccUrl); err == nil {
		return config, nil
	}
	return getConfigLocally()
}

func getConfigRemotely(hostname, ccUrl string) (*AuthboxConfig, error) {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ccUrl+"/config/"+hostname, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var config AuthboxConfig
	json.NewDecoder(resp.Body).Decode(&config)
	return &config, nil
}

func getConfigLocally() (*AuthboxConfig, error) {
	f, err := os.Open(os.Getenv("LOCAL_CONFIG_FILE"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var config AuthboxConfig
	json.NewDecoder(f).Decode(&config)
	return &config, nil
}

func BadgeReader(c badgeReaderConfig) (*DeviceRet[string], error) {
	device, err := findBadgeReader(c)
	if err != nil {
		return nil, err
	}
	if err := device.Grab(); err != nil {
		return nil, err
	}
	events := make(chan string)
	looper := func() {
		keys := make(chan *evdev.InputEvent)
		go func() {
			for {
				e, err := device.ReadOne()
				if err != nil {
					slog.Warn("badge: could not read event: %v", err)
				}
				if e.Type != evdev.EV_KEY {
					continue
				}
				if e.Value == 0 {
					continue
				}
				keys <- e
			}
		}()
		timeout := time.NewTimer(0)
		timeout.Stop()
		s := ""
		cap := false
		for {
			select {
			case e := <-keys:
				timeout.Reset(time.Duration(c.TimeoutMs) * time.Millisecond)
				switch {
				case e.Code == evdev.KEY_LEFTSHIFT, e.Code == evdev.KEY_RIGHTSHIFT:
					cap = true
				case e.Code == evdev.KEY_ENTER:
					slog.Debug("badge: badged", slog.String("id", s))
					events <- s
					s = ""
					cap = false
				case func() bool { _, ok := usKeyMap[e.Code]; return ok }():
					if cap {
						s += usKeyMap[e.Code].cap
					} else {
						s += usKeyMap[e.Code].normal
					}
					cap = false
				default:
					c := string(strings.TrimPrefix(e.CodeName(), "KEY_")[0])
					if cap {
						s += strings.ToUpper(c)
					} else {
						s += strings.ToLower(c)
					}
					cap = false
				}
			case <-timeout.C:
				s = ""
				cap = false
				timeout.Stop()
			}
		}
	}
	announce := func(name string, topic string) interface{} {
		return struct {
			Topic         string     `json:"topic"`
			ValueTemplate string     `json:"value_template,omitempty"`
			Device        MqttDevice `json:"device"`
		}{
			Topic:         topic + "/" + name + "/badged",
			ValueTemplate: "{{value}}",
			Device:        MqttDevice{Name: "Badge reader on " + name},
		}
	}
	return &DeviceRet[string]{
		Looper: looper,
		Events: events,
		OnEvent: func(badgeId string, name string, publish PublishFunc) {
			publish(name+"/badged", badgeId)
		},
		Discovery: MqttDiscovery{
			Component: "tag",
			Id:        "badge_reader",
			Announce:  announce,
		},
	}, nil
}

func CurrentSensing(c currentSensingConfig) (*DeviceRet[bool], error) {
	chip, err := findGpioChip()
	if err != nil {
		return nil, err
	}
	events := make(chan bool)
	line, err := chip.RequestLine(
		c.Pin,
		gpiocdev.AsInput,
		gpiocdev.WithPullDown,
		gpiocdev.WithBothEdges,
		gpiocdev.DebounceOption(time.Duration(c.DebounceMs)*time.Millisecond),
		gpiocdev.WithEventHandler(func(le gpiocdev.LineEvent) {
			high := false
			if le.Type == gpiocdev.LineEventRisingEdge {
				high = true
			}
			slog.Debug("gpio: pin transition", slog.Int("pin", c.Pin), slog.Bool("high", high))
			events <- high
		}))
	_ = line
	if err != nil {
		return nil, err
	}
	looper := func() {
		for {
			time.Sleep(time.Second * 60)
		}
	}
	return &DeviceRet[bool]{
		Looper: looper,
		Events: events,
		OnEvent: func(isHigh bool, name string, publish func(string, interface{})) {
			publish(name+"/current", map[bool]string{false: "0", true: "42"}[isHigh])
		},
		Discovery: MqttDiscovery{
			Component: "switch",
			Id:        "current_sensor",
			Announce: func(name, topic string) interface{} {
				return struct {
					Device      MqttDevice `json:"device"`
					DeviceClass string     `json:"device_class"`
					StateTopic  string     `json:"state_topic"`
					Unit        string     `json:"unit_of_measurement"`
				}{
					Device:      MqttDevice{Name: "Current sensor on " + name},
					DeviceClass: "current",
					StateTopic:  topic + "/" + name + "/current",
					Unit:        "A",
				}
			},
		},
	}, nil
}

func Relay(c relayConfig, isOn <-chan bool) (*DeviceRet[bool], error) {
	chip, err := findGpioChip()
	if err != nil {
		return nil, err
	}
	line, err := chip.RequestLine(c.Pin, gpiocdev.AsOutput(0))
	if err != nil {
		return nil, err
	}
	looper := func() {
		for {
			select {
			case on := <-isOn:
				line.SetValue(map[bool]int{false: 0, true: 1}[on])
			}
		}
	}
	discovery := MqttDiscovery{
		Component: "switch",
		Id:        "relay",
		Announce: func(name, topic string) interface{} {
			return struct {
				Device       MqttDevice `json:"device"`
				CommandTopic string     `json:"command_topic"`
				StateTopic   string     `json:"state_topic"`
			}{
				Device:       MqttDevice{Name: "Relay on " + name},
				CommandTopic: topic + "/" + name + "/relay/set", // ignored, read-only
				StateTopic:   topic + "/" + name + "/relay",
			}
		},
	}
	return &DeviceRet[bool]{
		Looper: looper,
		Events: nil,
		OnEvent: func(isOn bool, name string, publish func(string, interface{})) {
			publish(name+"/relay", map[bool]string{false: "OFF", true: "ON"}[isOn])
		},
		Discovery: discovery,
	}, nil
}

func Blinker(c ledConfig, piLedName string, mode <-chan interface{}) (func(), error) {
	setPiLed := func(isOn bool) {
		os.WriteFile("/sys/class/leds/"+piLedName+"/brightness", []byte(map[bool]string{false: "0", true: "1"}[isOn]), 0)
	}
	gpio, err := findGpioChip()
	if err != nil {
		return nil, err
	}
	line, err := gpio.RequestLine(c.Pin, gpiocdev.AsOutput(0))
	if err != nil {
		return nil, err
	}
	return func() {
		timer := time.NewTicker(time.Millisecond)
		timer.Stop()
		isOn := false
		for {
			select {
			case m := <-mode:
				switch mm := m.(type) {
				case LedStatic:
					timer.Stop()
					line.SetValue(map[bool]int{false: 0, true: 1}[mm.On])
					go setPiLed(mm.On)
				case LedModeBlink:
					isOn = false
					line.SetValue(0)
					go setPiLed(isOn)
					timer.Reset(mm.Interval)
				}
			case <-timer.C:
				isOn = !isOn
				line.SetValue(map[bool]int{false: 0, true: 1}[isOn])
				go setPiLed(isOn)
			}
		}
	}, nil
}

type MqttEvent struct {
	DisconnectedError error
}

func MqttBroker(name string, c mqttConfig, discoveries []MqttDiscovery) (func(), <-chan MqttEvent, PublishFunc) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(c.Broker)
	opts.SetClientID("authbox/" + name)
	opts.SetAutoReconnect(true)
	opts.SetConnectTimeout(time.Second * 2)
	opts.SetConnectRetryInterval(time.Second * 2)

	events := make(chan MqttEvent)

	sendDiscoveries := func(mc mqtt.Client) {
		for _, d := range discoveries {
			bytes, err := json.Marshal(d.Announce(name, c.Topic))
			if err != nil {
				slog.Error("could not marshall JSON for Home Assistant discovery", slog.Any("error", err))
				continue
			}
			if t := mc.Publish("homeassistant/"+d.Component+"/"+d.Id+"/config", 0, true, string(bytes)); t.Wait() && t.Error() != nil {
				slog.Error("error publishing Home Assistant discovery", slog.Any("error", t.Error()))
			}
		}
	}

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		events <- MqttEvent{DisconnectedError: err}
	})
	opts.SetOnConnectHandler(func(mc mqtt.Client) {
		events <- MqttEvent{DisconnectedError: nil}
		sendDiscoveries(mc)
	})

	mc := mqtt.NewClient(opts)

	looper := func() {
		for {
			if t := mc.Connect(); t.Wait() && t.Error() != nil {
				events <- MqttEvent{DisconnectedError: t.Error()}
				time.Sleep(time.Second * 5)
			} else {
				return
			}
		}
	}

	publish := func(topic string, payload interface{}) {
		if t := mc.Publish(c.Topic+"/"+topic, 0, false, payload); t.Wait() && t.Error() != nil {
			slog.Error("could not publish to mqtt", slog.Any("error", t.Error()))
		}
	}

	return looper, events, publish
}

// Sends a HTTP request to check for badge access.
func BadgeAuth(c badgeAuthConfig, badgeId string, action string) error {
	t, err := template.New("url").Parse(c.UrlTemplate)
	if err != nil {
		return err
	}
	var url strings.Builder
	err = t.Execute(&url, map[string]interface{}{
		"badge":    badgeId,
		"state":    action,
		"duration": c.UsageMinutes,
	})
	if err != nil {
		return err
	}
	resp, err := http.Post(url.String(), "text/plain", strings.NewReader(""))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var reason []byte
		if reason, err = io.ReadAll(io.LimitReader(resp.Body, 256)); err != nil {
			reason = []byte("(can't decode body)")
		}
		return errors.New("error authenticating badge: " + string(reason))
	}
	return nil
}

// Finds the badge reader input device by vendor & product IDs.
func findBadgeReader(c badgeReaderConfig) (*evdev.InputDevice, error) {
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		return nil, err
	}
	for _, d := range paths {
		device, err := evdev.Open(d.Path)
		if err != nil {
			println("onoes open")
			return nil, err
		}
		inpId, err := device.InputID()
		if err != nil {
			return nil, err
		}
		if d.Name == c.Name || (inpId.Vendor == c.Vendor && inpId.Product == c.Product) {
			return device, nil
		}
	}
	return nil, errors.New(fmt.Sprintf("no badge reader found amongst %d devices with ID %04x:%04x", len(paths), c.Vendor, c.Product))
}

// Finds the GPIO chip by label prefix.
func findGpioChip() (*gpiocdev.Chip, error) {
	paths, err := filepath.Glob("/dev/gpiochip*")
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		c, err := gpiocdev.NewChip(p, gpiocdev.WithConsumer("gauthbox"))
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(c.Label, GPIO_WANTED_PREFIX) {
			return c, err
		}
	}
	return nil, errors.New(fmt.Sprintf("no GPIO chip found amongst %d devices with prefix '%s'", len(paths), GPIO_WANTED_PREFIX))
}

// sdNotify sends a message to systemd notify socket.
func SdNotify(state string) (bool, error) {
	socketAddr := &net.UnixAddr{
		Name: os.Getenv("NOTIFY_SOCKET"),
		Net:  "unixgram",
	}
	if socketAddr.Name == "" {
		return false, nil
	}
	conn, err := net.DialUnix(socketAddr.Net, nil, socketAddr)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

var usKeyMap = map[evdev.EvCode]struct {
	normal string
	cap    string
}{
	evdev.KEY_1:          {"1", "!"},
	evdev.KEY_2:          {"2", "@"},
	evdev.KEY_3:          {"3", "#"},
	evdev.KEY_4:          {"4", "$"},
	evdev.KEY_5:          {"5", "%"},
	evdev.KEY_6:          {"6", "^"},
	evdev.KEY_7:          {"7", "&"},
	evdev.KEY_8:          {"8", "*"},
	evdev.KEY_9:          {"9", "("},
	evdev.KEY_0:          {"0", ")"},
	evdev.KEY_MINUS:      {"-", "_"},
	evdev.KEY_EQUAL:      {"=", "+"},
	evdev.KEY_LEFTBRACE:  {"[", "{"},
	evdev.KEY_RIGHTBRACE: {"]", "}"},
	evdev.KEY_SEMICOLON:  {";", ":"},
	evdev.KEY_APOSTROPHE: {"'", "\""},
	evdev.KEY_GRAVE:      {"`", "~"},
	evdev.KEY_BACKSLASH:  {"\\", "|"},
	evdev.KEY_COMMA:      {",", "<"},
	evdev.KEY_DOT:        {".", ">"},
	evdev.KEY_SLASH:      {"/", "?"},
	evdev.KEY_SPACE:      {" ", " "},
}
