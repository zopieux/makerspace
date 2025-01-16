package gauthbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
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
	// .badgeId, .state, .duration
	UrlTemplate  string `json:"url_template"`
	UsageMinutes uint32 `json:"usage_duration_minutes"`
}

type relayConfig struct {
	Pin       int  `json:"pin"`
	ActiveLow bool `json:"active_low"`
	Debounce  int  `json:"debounce_ms"`
}

type currentSensingConfig struct {
	Pin        int    `json:"pin"`
	ActiveLow  bool   `json:"active_low"`
	DebounceMs int    `json:"debounce_ms"`
	Bias       string `json:"bias"`
}

type mqttConfig struct {
	Broker    string `json:"broker"`
	BaseTopic string `json:"topic"`
}

type ledConfig struct {
	Pin       int  `json:"pin"`
	ActiveLow bool `json:"active_low"`
}

type LedStatic struct {
	On bool
}
type LedBlink struct {
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

type MqttComponentDiscoveryFunc func(baseTopic string) HaComponent
type MqttComponentPublishFunc func(payload interface{}) (string, interface{})
type MqttComponent struct {
	Id        string
	Component MqttComponentDiscoveryFunc
	Publish   MqttComponentPublishFunc
}
type MqttDevice struct {
	Name         string `json:"string,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
}
type DeviceRet[Event any] struct {
	Looper func()
	Events chan Event
	Mqtt   MqttComponent
}
type PublishFunc = func(b MqttComponent, payload interface{})

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

// Retrieves and parses the config from ccUrl.
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

// Retrieves the config from the local file at path $LOCAL_CONFIG_FILE.
func getConfigLocally() (*AuthboxConfig, error) {
	f, err := os.Open(os.Getenv("LOCAL_CONFIG_FILE"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var config AuthboxConfig
	if err := json.NewDecoder(f).Decode(&config); err != nil {
		return nil, err
	}
	return &config, nil
}

// Badge reader logic. The event stream yields ASCII badge IDs.
// MQTT: registers as a tag scanner.
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
					slog.Warn("badge: could not read event", slog.Any("err", err))
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
	announce := func(baseTopic string) HaComponent {
		return HaComponent{
			Name:       "Badged in",
			Platform:   "sensor",
			Icon:       "mdi:badge-account",
			BaseTopic:  baseTopic,
			StateTopic: "~/state",
		}
	}
	return &DeviceRet[string]{
		Looper: looper,
		Events: events,
		Mqtt: MqttComponent{
			Id:        "badge",
			Component: announce,
			Publish: func(badgeId interface{}) (string, interface{}) {
				return "/state", badgeId.(string)
			},
		},
	}, nil
}

// Current sensing logic (digital). The event stream yield high/low transitions.
// MQTT: registers as a switch with a 'current' device class. 0 Amps means no current, 42 Amps means some current.
func CurrentSensing(c currentSensingConfig) (*DeviceRet[bool], error) {
	chip, err := findGpioChip()
	if err != nil {
		return nil, err
	}
	events := make(chan bool)
	bias := gpiocdev.LineBiasPullDown
	if c.Bias == "pull_up" {
		bias = gpiocdev.LineBiasPullUp
	}
	line, err := chip.RequestLine(
		c.Pin,
		gpiocdev.AsInput,
		bias,
		gpiocdev.WithBothEdges,
		gpiocdev.DebounceOption(time.Duration(c.DebounceMs)*time.Millisecond),
		gpiocdev.WithEventHandler(func(le gpiocdev.LineEvent) {
			high := false
			if le.Type == gpiocdev.LineEventRisingEdge {
				high = true
			}
			if c.ActiveLow {
				high = !high
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
		Mqtt: MqttComponent{
			Id: "current",
			Component: func(baseTopic string) HaComponent {
				return HaComponent{
					Name:              "Current sensing",
					Platform:          "sensor",
					DeviceClass:       "current",
					UnitOfMeasurement: "A",
					BaseTopic:         baseTopic,
					StateTopic:        "~/state",
				}
			},
			Publish: func(isHigh interface{}) (string, interface{}) {
				// Dummy non-zero value (10 Amperes) when on.
				return "/state", map[bool]string{false: "0", true: "10"}[isHigh.(bool)]
			},
		},
	}, nil
}

// Sets the line value according to 'on'.
// The high/low logic if inverted if activeLow is true.
func setLineValue(activeLow bool, line *gpiocdev.Line, on bool) error {
	value := on
	if activeLow {
		value = !value
	}
	return line.SetValue(map[bool]int{false: 0, true: 1}[value])
}

// Relay logic. Switches a GPIO pin according to 'isOn' booleans.
// MQTT: registers as a switch.
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
				setLineValue(c.ActiveLow, line, on)
			}
		}
	}
	discovery := MqttComponent{
		Id: "relay",
		Component: func(baseTopic string) HaComponent {
			return HaComponent{
				Name:         "Relay",
				Platform:     "binary_sensor",
				DeviceClass:  "power",
				Icon:         "mdi:power-socket-ch",
				BaseTopic:    baseTopic,
				StateTopic:   "~/state",
				CommandTopic: "~/set", // Ignored, read-only.
			}
		},
		Publish: func(isOn interface{}) (string, interface{}) {
			return "/state", map[bool]string{false: "OFF", true: "ON"}[isOn.(bool)]
		},
	}
	return &DeviceRet[bool]{
		Looper: looper,
		Events: nil,
		Mqtt:   discovery,
	}, nil
}

// Blinker utility to set a GPIO LED in either static or blink mode.
// To change the state, send either LedStatic{On: bool} or LedBlink{Interval: Duration} to chan 'mode'.
// If sysLedName is non-empty, this also controls the on-board LED at /sys/class/leds/<sysLedName>.
func Blinker(c ledConfig, sysLedName string, mode <-chan interface{}) (func(), error) {
	if sysLedName != "" {
		os.WriteFile("/sys/class/leds/"+sysLedName+"/trigger", []byte("none"), 0)
	}
	setPiLed := func(isOn bool) {
		if sysLedName != "" {
			os.WriteFile("/sys/class/leds/"+sysLedName+"/brightness", []byte(map[bool]string{false: "0", true: "1"}[isOn]), 0)
		}
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
					setLineValue(c.ActiveLow, line, mm.On)
					go setPiLed(mm.On)
				case LedBlink:
					isOn = false
					setLineValue(c.ActiveLow, line, false)
					go setPiLed(isOn)
					timer.Reset(mm.Interval)
				}
			case <-timer.C:
				isOn = !isOn
				setLineValue(c.ActiveLow, line, isOn)
				go setPiLed(isOn)
			}
		}
	}, nil
}

type MqttEvent struct {
	DisconnectedError error
}

// https://www.home-assistant.io/integrations/mqtt/#supported-abbreviations-in-mqtt-discovery-messages
type haDevice struct {
	ConfigurationUrl string     `json:"configuration_url,omitempty"`
	Connections      [][]string `json:"connections,omitempty"`
	Identifiers      []string   `json:"identifiers,omitempty"`
	Name             string     `json:"name,omitempty"`
	Manufacturer     string     `json:"manufacturer,omitempty"`
	Model            string     `json:"model,omitempty"`
	ModelId          string     `json:"model_id,omitempty"`
	HwVersion        string     `json:"hw_version,omitempty"`
	SwVersion        string     `json:"sw_version,omitempty"`
	SuggestedArea    string     `json:"suggested_area,omitempty"`
	SerialNumber     string     `json:"serial_number,omitempty"`
}
type haOrigin struct {
	Name       string `json:"name"`
	SwVersion  string `json:"sw,omitempty"`
	SupportUrl string `json:"url,omitempty"`
}
type HaComponent struct {
	Platform          string `json:"p"` // Required.
	Type              string `json:"type,omitempty"`
	SubType           string `json:"subtype,omitempty"`
	DeviceClass       string `json:"device_class,omitempty"`
	AutomationType    string `json:"automation_type,omitempty"`
	BaseTopic         string `json:"~,omitempty"`
	Topic             string `json:"topic,omitempty"`
	CommandTopic      string `json:"command_topic,omitempty"`
	StateTopic        string `json:"state_topic,omitempty"`
	Icon              string `json:"icon,omitempty"`
	UnitOfMeasurement string `json:"unit_of_measurement,omitempty"`
	ValueTemplate     string `json:"value_template,omitempty"`
	UniqueId          string `json:"unique_id,omitempty"`
	Mode              string `json:"mode,omitempty"`
	Name              string `json:"name,omitempty"`
}
type haDeviceConfig struct {
	Device     haDevice               `json:"device"`
	Origin     haOrigin               `json:"origin"`
	Components map[string]HaComponent `json:"components"`
}

// Publish/subscribe to MQTT logic. At connect time, publishes the Home Assistant config discovery message.
// Use the returned PublishFunc to publish messages using the configured topic prefix.
func MqttBroker(name string, c mqttConfig, discoveries []MqttComponent) (func(), <-chan MqttEvent, PublishFunc) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(c.Broker)
	opts.SetClientID("authbox/" + name)
	opts.SetAutoReconnect(true)
	opts.SetConnectTimeout(time.Second * 2)
	opts.SetConnectRetryInterval(time.Second * 2)

	haDeviceId := "authbox_" + name
	haConfigTopic := "homeassistant/device/" + haDeviceId + "/config"
	deviceTopicPrefix := c.BaseTopic + "/" + haDeviceId

	componentTopic := func(componentId string) string {
		return deviceTopicPrefix + "/" + componentId
	}

	events := make(chan MqttEvent)

	sendDeviceConfig := func(mc mqtt.Client) {
		components := map[string]HaComponent{}
		for _, d := range discoveries {
			uniqueId := haDeviceId + "_" + d.Id
			c := HaComponent(d.Component(componentTopic(d.Id)))
			c.UniqueId = uniqueId
			components[uniqueId] = c
		}
		devConfig := haDeviceConfig{
			Device: haDevice{
				Name:        name,
				Identifiers: []string{haDeviceId},
				// TODO: Don't hard-code.
				Model:        "Button-less (Woodshop)",
				Manufacturer: "Zurich Makerspace Organizers",
				SwVersion:    "gauthbox ⋅ v1",
				HwVersion:    "Pi 3B+ PoE",
			},
			Origin: haOrigin{
				Name: name,
			},
			Components: components,
		}
		bytes, err := json.Marshal(devConfig)
		if err != nil {
			slog.Error("could not marshall JSON for Home Assistant discovery config", slog.Any("error", err))
			return
		}
		if t := mc.Publish(haConfigTopic, 0, true, string(bytes)); t.Wait() && t.Error() != nil {
			slog.Error("error publishing Home Assistant discovery config to MQTT", slog.Any("error", t.Error()))
		}
	}

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		events <- MqttEvent{DisconnectedError: err}
	})
	opts.SetOnConnectHandler(func(mc mqtt.Client) {
		events <- MqttEvent{DisconnectedError: nil}
		sendDeviceConfig(mc)
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

	publish := func(b MqttComponent, payload interface{}) {
		topicSuffix, pubPayload := b.Publish(payload)
		topic := componentTopic(b.Id) + topicSuffix
		if t := mc.Publish(topic, 0, false, pubPayload); t.Wait() && t.Error() != nil {
			slog.Error("error publishing component MQTT message", slog.String("component", b.Id), slog.Any("error", t.Error()))
		}
	}

	return looper, events, publish
}

// Sends a HTTP request to check for badge access.
func BadgeAuth(c badgeAuthConfig, badgeId string, state string) error {
	t, err := template.New("url").Parse(c.UrlTemplate)
	if err != nil {
		return err
	}
	var url strings.Builder
	err = t.Execute(&url, map[string]interface{}{
		"badgeId":  badgeId,
		"state":    state,
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

// Finds the badge reader input device by either name or numeric vendor & product IDs.
func findBadgeReader(c badgeReaderConfig) (*evdev.InputDevice, error) {
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		return nil, err
	}
	for _, d := range paths {
		device, err := evdev.Open(d.Path)
		if err != nil {
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
	return nil, fmt.Errorf("no badge reader found amongst %d devices with ID %04x:%04x", len(paths), c.Vendor, c.Product)
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
	return nil, fmt.Errorf("no GPIO chip found amongst %d devices with prefix '%s'", len(paths), GPIO_WANTED_PREFIX)
}

// Sends a message to systemd notify socket.
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
