package gauthbox

import (
	"context"
	"encoding/json"
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

type BadgeReaderConfig struct {
	Vendor    uint16 `json:"vendor,omitempty"`
	Product   uint16 `json:"product,omitempty"`
	Name      string `json:"name,omitempty"`
	TimeoutMs uint32 `json:"timeout_ms"`
}

type BadgeAuthConfig struct {
	// .badgeId, .state, .duration
	UrlTemplate  string `json:"url_template"`
	UsageMinutes uint32 `json:"usage_duration_minutes"`
}

type RelayConfig struct {
	Pin       int  `json:"pin"`
	ActiveLow bool `json:"active_low"`
	Debounce  int  `json:"debounce_ms"`
}

type CurrentSensingConfig struct {
	Pin        int    `json:"pin"`
	ActiveLow  bool   `json:"active_low"`
	DebounceMs int    `json:"debounce_ms"`
	Bias       string `json:"bias"`
}

type MqttConfig struct {
	Broker       string `json:"broker"`
	BaseTopic    string `json:"topic"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	SwVersion    string `json:"sw_version,omitempty"`
	HwVersion    string `json:"hw_version,omitempty"`
	DeviceId     string `json:"device_id,omitempty"`
	DeviceName   string `json:"device_name,omitempty"`
}

type LedConfig struct {
	Pin       int  `json:"pin"`
	ActiveLow bool `json:"active_low"`
}

type LedStatic struct {
	On bool
}
type LedBlink struct {
	Interval time.Duration
}

type UsbGadgetConfig struct {
	RclonePrefix      string `json:"rclone_prefix,omitempty"`
	MaxTransfer       string `json:"max_transfer,omitempty"`
	MaxSize           string `json:"max_size,omitempty"`
	MaxAge            string `json:"max_age,omitempty"`
	HaSensorModel     string `json:"ha_sensor_model,omitempty"`
	HaSensorManuf     string `json:"ha_sensor_manufacturer,omitempty"`
	UsbLabel          string `json:"usb_label,omitempty"`
	UsbSerialNumber   string `json:"usb_serial_number,omitempty"`
	UsbManufacturer   string `json:"usb_manufacturer,omitempty"`
	UsbProduct        string `json:"usb_product,omitempty"`
	UsbConfigName     string `json:"usb_config_name,omitempty"`
	UsbInquiryVendor  string `json:"usb_inquiry_vendor,omitempty"`
	UsbInquiryProduct string `json:"usb_inquiry_product,omitempty"`
	UsbSelectPin      *int   `json:"usb_select_pin,omitempty"`
	UsbEnablePin      *int   `json:"usb_enable_pin,omitempty"`
	LogoutPin         *int   `json:"logout_pin,omitempty"`
	ReloadPin         *int   `json:"reload_pin,omitempty"`
}

type ButtonConfig struct {
	Pin        int    `json:"pin"`
	ActiveLow  bool   `json:"active_low"`
	DebounceMs int    `json:"debounce_ms"`
	Bias       string `json:"bias"`
}

type AuthboxConfig struct {
	MqttBroker     *MqttConfig           `json:"mqtt,omitempty"`
	BadgeReader    *BadgeReaderConfig    `json:"badge_reader,omitempty"`
	BadgeAuth      *BadgeAuthConfig      `json:"badge_auth,omitempty"`
	CurrentSensing *CurrentSensingConfig `json:"current_sensing,omitempty"`
	Relay          *RelayConfig          `json:"relay,omitempty"`
	GreenLed       *LedConfig            `json:"green_led,omitempty"`
	RedLed         *LedConfig            `json:"red_led,omitempty"`
	IdleSeconds    uint32                `json:"idle_duration_s,omitempty"`
	Gadget         *UsbGadgetConfig      `json:"gadget,omitempty"`
	OnButton       *ButtonConfig         `json:"on_button,omitempty"`
	OffButton      *ButtonConfig         `json:"off_button,omitempty"`
}

type BadgingChan = <-chan string
type CurrentSensingChan = <-chan bool
type RelayIsOnChan = <-chan bool

type MqttComponentDiscoveryFunc func(baseTopic string) HaComponent
type MqttComponentPublishFunc func(payload interface{}) (string, interface{})
type MqttSub struct {
	Topic    string
	Callback func(topic string, payload string)
}
type MqttComponentSubscribeFunc func() []MqttSub
type MqttComponent struct {
	Id        string
	Component MqttComponentDiscoveryFunc
	Publish   MqttComponentPublishFunc
	Subscribe []MqttSub
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
	return GetConfigForHostname(ccUrl, hostname)
}

// Retrieves the config from command & control for a specific hostname, falling back to the SD card if
// that fails.
func GetConfigForHostname(ccUrl string, hostname string) (*AuthboxConfig, error) {
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
	if !(200 <= resp.StatusCode && resp.StatusCode < 300) {
		return nil, fmt.Errorf("HTTP error requesting config: %d (%s)", resp.StatusCode, resp.Status)
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

// For mocking purposes.
var BadgeReaderFn = BadgeReader

// Badge reader logic. The event stream yields ASCII badge IDs.
// MQTT: registers as a tag scanner.
func BadgeReader(c BadgeReaderConfig) (*DeviceRet[string], error) {
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
			defer close(keys)
			for {
				e, err := device.ReadOne()
				if err != nil {
					slog.Warn("badge: could not read event", slog.Any("err", err))
					return
				}
				if e == nil {
					continue
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
			case e, ok := <-keys:
				if !ok {
					close(events)
					return
				}
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
			Publish: func(badgedIn interface{}) (string, interface{}) {
				return "/state", map[bool]string{false: "OFF", true: "ON"}[badgedIn.(bool)]
			},
		},
	}, nil
}

type GpioLine interface {
	SetValue(val int) error
	Close() error
}

var (
	RequestOutputPinFn = func(pin int, initVal int) (GpioLine, error) {
		chip, err := findGpioChip()
		if err != nil {
			return nil, err
		}
		return chip.RequestLine(pin, gpiocdev.AsOutput(initVal))
	}

	RequestInputPinFn = func(pin int, bias string, debounce time.Duration, callback func(bool)) (io.Closer, error) {
		chip, err := findGpioChip()
		if err != nil {
			return nil, err
		}
		gLineBias := gpiocdev.LineBiasPullDown
		if bias == "pull_up" {
			gLineBias = gpiocdev.LineBiasPullUp
		}
		line, err := chip.RequestLine(
			pin,
			gpiocdev.AsInput,
			gLineBias,
			gpiocdev.WithBothEdges,
			gpiocdev.DebounceOption(debounce),
			gpiocdev.WithEventHandler(func(le gpiocdev.LineEvent) {
				high := (le.Type == gpiocdev.LineEventRisingEdge)
				callback(high)
			}),
		)
		return line, err
	}

	WriteSysfsLedFn = func(sysLedName string, brightness string) error {
		if sysLedName == "" {
			return nil
		}
		return os.WriteFile("/sys/class/leds/"+sysLedName+"/brightness", []byte(brightness), 0)
	}

	WriteSysfsLedTriggerFn = func(sysLedName string, trigger string) error {
		if sysLedName == "" {
			return nil
		}
		return os.WriteFile("/sys/class/leds/"+sysLedName+"/trigger", []byte(trigger), 0)
	}

	SdNotifyFn = func(state string) (bool, error) {
		socketAddr := &net.UnixAddr{
			Name: os.Getenv("NOTIFY_SOCKET"),
			Net:  "unixgram",
		}
		if socketAddr.Name == "" {
			return false, nil
		}
		conn, err := net.DialUnix(socketAddr.Net, nil, socketAddr)
		if err != nil {
			return false, fmt.Errorf("SdNotify: error dialing socket: %w", err)
		}
		defer conn.Close()
		if _, err = conn.Write([]byte(state)); err != nil {
			return false, fmt.Errorf("SdNotify: error writing to socket: %w", err)
		}
		return true, nil
	}
)

// Current sensing logic (digital). The event stream yield high/low transitions.
// MQTT: registers as a switch with a 'current' device class. 0 Amps means no current, 42 Amps means some current.
func CurrentSensing(c CurrentSensingConfig) (*DeviceRet[bool], error) {
	events := make(chan bool)
	line, err := RequestInputPinFn(c.Pin, c.Bias, time.Duration(c.DebounceMs)*time.Millisecond, func(high bool) {
		if c.ActiveLow {
			high = !high
		}
		slog.Debug("gpio: pin transition", slog.Int("pin", c.Pin), slog.Bool("high", high))
		events <- high
	})
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
					StateClass:        "measurement", // For long-term retention.
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
func setLineValue(activeLow bool, line GpioLine, on bool) error {
	value := on
	if activeLow {
		value = !value
	}
	return line.SetValue(map[bool]int{false: 0, true: 1}[value])
}

// Relay logic. Switches a GPIO pin according to 'isOn' booleans.
// MQTT: registers as a switch.
func Relay(c RelayConfig, isOn <-chan bool) (*DeviceRet[bool], error) {
	line, err := RequestOutputPinFn(c.Pin, 0)
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
				CommandTopic: "~/set",       // Ignored, read-only.
				StateClass:   "measurement", // For long-term retention.
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

// Out-of-order toggle. Prevents badging-in when locked (off).
func AccessAllowed(isAllowed chan<- bool) (*DeviceRet[bool], error) {
	discovery := MqttComponent{
		Id: "access",
		Component: func(baseTopic string) HaComponent {
			return HaComponent{
				Name:         "Access allowed",
				Platform:     "switch",
				DeviceClass:  "switch",
				BaseTopic:    baseTopic,
				Icon:         "mdi:lock-open-variant",
				CommandTopic: "~/set",
				PayloadOn:    "ON",
				PayloadOff:   "OFF",
				Retain:       true, // We want retained values so we can get the value upon boot.
			}
		},
		Subscribe: []MqttSub{{
			Topic: "/set",
			Callback: func(topic string, payload string) {
				isAllowed <- strings.ToLower(payload) == "on"
			},
		}},
	}
	looper := func() {
		for {
			time.Sleep(time.Second * 60)
		}
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
func Blinker(c LedConfig, sysLedName string, mode <-chan interface{}) (func(), error) {
	if sysLedName != "" {
		WriteSysfsLedTriggerFn(sysLedName, "none")
	}
	setPiLed := func(isOn bool) {
		if sysLedName != "" {
			brightness := map[bool]string{false: "0", true: "1"}[isOn]
			WriteSysfsLedFn(sysLedName, brightness)
		}
	}
	line, err := RequestOutputPinFn(c.Pin, 0)
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

type MqttConnected struct{}
type MqttDisonnected struct{ Error error }
type MqttResetRequest struct{}

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
	Platform          string   `json:"p"` // Required.
	AvailabilityTopic string   `json:"availability_topic,omitempty"`
	Type              string   `json:"type,omitempty"`
	SubType           string   `json:"subtype,omitempty"`
	DeviceClass       string   `json:"device_class,omitempty"`
	AutomationType    string   `json:"automation_type,omitempty"`
	BaseTopic         string   `json:"~,omitempty"`
	Topic             string   `json:"topic,omitempty"`
	CommandTopic      string   `json:"command_topic,omitempty"`
	PayloadOn         string   `json:"payload_on,omitempty"`
	PayloadOff        string   `json:"payload_off,omitempty"`
	Retain            bool     `json:"retain,omitempty"`
	StateTopic        string   `json:"state_topic,omitempty"`
	StateClass        string   `json:"state_class,omitempty"`
	Icon              string   `json:"icon,omitempty"`
	UnitOfMeasurement string   `json:"unit_of_measurement,omitempty"`
	ValueTemplate     string   `json:"value_template,omitempty"`
	UniqueId          string   `json:"unique_id,omitempty"`
	Mode              string   `json:"mode,omitempty"`
	Name              string   `json:"name,omitempty"`
	Options           []string `json:"options,omitempty"`
}
type haDeviceConfig struct {
	Device     haDevice               `json:"device"`
	Origin     haOrigin               `json:"origin"`
	Components map[string]HaComponent `json:"components"`
}

// Publish/subscribe to MQTT logic. At connect time, publishes the Home Assistant config discovery message.
// Use the returned PublishFunc to publish messages using the configured topic prefix.
func MqttBroker(c MqttConfig, comps []MqttComponent) (func(), <-chan interface{}, PublishFunc) {
	name := c.DeviceId
	if name == "" {
		name = "unknown"
	}
	haDeviceId := "authbox_" + name

	deviceTopicPrefix := c.BaseTopic + "/" + haDeviceId
	deviceAvailabilityTopic := deviceTopicPrefix + "/LWT"
	resetTopic := c.BaseTopic + "/reset"

	haConfigTopic := "homeassistant/device/" + haDeviceId + "/config"

	opts := mqtt.NewClientOptions()
	opts.AddBroker(c.Broker)
	opts.SetClientID("authbox/" + name)
	opts.SetAutoReconnect(true)
	opts.SetConnectTimeout(time.Second * 2)
	opts.SetConnectRetryInterval(time.Second * 2)
	opts.SetWill(deviceAvailabilityTopic, "offline", 0, true)

	componentTopic := func(componentId string) string {
		return deviceTopicPrefix + "/" + componentId
	}

	events := make(chan interface{})

	sendDeviceConfig := func(mc mqtt.Client) {
		components := map[string]HaComponent{}
		for _, d := range comps {
			uniqueId := haDeviceId + "_" + d.Id
			c := HaComponent(d.Component(componentTopic(d.Id)))
			c.UniqueId = uniqueId
			c.AvailabilityTopic = deviceAvailabilityTopic
			components[uniqueId] = c
		}
		deviceName := c.DeviceName
		if deviceName == "" {
			deviceName = name
		}
		devConfig := haDeviceConfig{
			Device: haDevice{
				Name:         deviceName,
				Identifiers:  []string{haDeviceId},
				Model:        c.Model,
				Manufacturer: c.Manufacturer,
				SwVersion:    c.SwVersion,
				HwVersion:    c.HwVersion,
			},
			Origin: haOrigin{
				Name: deviceName,
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
		events <- MqttDisonnected{Error: err}
	})
	opts.SetOnConnectHandler(func(mc mqtt.Client) {
		events <- MqttConnected{}
		mc.Subscribe(resetTopic, 0, func(_ mqtt.Client, m mqtt.Message) {
			payload := string(m.Payload())
			// Empty payload means all, otherwise only the requested name is reset.
			if payload == "" || payload == name {
				events <- MqttResetRequest{}
			}
		})
		sendDeviceConfig(mc)
		if t := mc.Publish(deviceAvailabilityTopic, 0, true, "online"); t.Wait() && t.Error() != nil {
			slog.Error("error publishing availability", slog.Any("error", t.Error()))
		}
		for _, d := range comps {
			for _, sub := range d.Subscribe {
				mc.Subscribe(componentTopic(d.Id)+sub.Topic, 0, func(c mqtt.Client, m mqtt.Message) {
					sub.Callback(m.Topic(), string(m.Payload()))
				})
			}
		}
	})

	mc := mqtt.NewClient(opts)

	looper := func() {
		for {
			if t := mc.Connect(); t.Wait() && t.Error() != nil {
				events <- MqttDisonnected{Error: t.Error()}
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
// Returns the authenticated username if present in the response headers.
func BadgeAuth(c BadgeAuthConfig, badgeId string, state string) (string, error) {
	t, err := template.New("url").Parse(c.UrlTemplate)
	if err != nil {
		return "", fmt.Errorf("BadgeAuth: error parsing URL template '%s': %w", c.UrlTemplate, err)
	}
	var url strings.Builder
	err = t.Execute(&url, map[string]interface{}{
		"badgeId":  badgeId,
		"state":    state,
		"duration": c.UsageMinutes,
	})
	if err != nil {
		return "", fmt.Errorf("BadgeAuth: error executing URL template: %w", err)
	}
	resp, err := http.Post(url.String(), "text/plain", strings.NewReader(""))
	if err != nil {
		return "", fmt.Errorf("BadgeAuth: error making HTTP request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var reason []byte
		if reason, err = io.ReadAll(io.LimitReader(resp.Body, 256)); err != nil {
			reason = []byte("(can't decode body)")
		}
		return "", fmt.Errorf("BadgeAuth: error authenticating badge (HTTP %d): '%s'", resp.StatusCode, string(reason))
	}
	username := resp.Header.Get("x-makerspace-username")
	if username == "" {
		return "", fmt.Errorf("BadgeAuth: no username in response headers for badge '%s'", badgeId)
	}
	return username, nil
}

// Finds the badge reader input device by either name or numeric vendor & product IDs.
func findBadgeReader(c BadgeReaderConfig) (*evdev.InputDevice, error) {
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		return nil, fmt.Errorf("findBadgeReader: error listing device paths: %w", err)
	}
	for _, d := range paths {
		device, err := evdev.Open(d.Path)
		if err != nil {
			return nil, fmt.Errorf("findBadgeReader: error opening device '%s': %w", d.Path, err)
		}
		inpId, err := device.InputID()
		if err != nil {
			return nil, fmt.Errorf("findBadgeReader: error getting input ID for device '%s': %w", d.Path, err)
		}
		if d.Name == c.Name || (inpId.Vendor == c.Vendor && inpId.Product == c.Product) {
			return device, nil
		}
	}
	return nil, fmt.Errorf("findBadgeReader: no badge reader found amongst %d devices with name '%s' and ID %04x:%04x", len(paths), c.Name, c.Vendor, c.Product)
}

// Finds the GPIO chip by label prefix.
func findGpioChip() (*gpiocdev.Chip, error) {
	paths, err := filepath.Glob("/dev/gpiochip*")
	if err != nil {
		return nil, fmt.Errorf("findGpioChip: error listing device paths: %w", err)
	}
	for _, p := range paths {
		c, err := gpiocdev.NewChip(p, gpiocdev.WithConsumer("gauthbox"))
		if err != nil {
			return nil, fmt.Errorf("findGpioChip: error opening device '%s': %w", p, err)
		}
		if strings.HasPrefix(c.Label, GPIO_WANTED_PREFIX) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("findGpioChip: no GPIO chip found amongst %d devices with prefix '%s'", len(paths), GPIO_WANTED_PREFIX)
}

// Sends a message to systemd notify socket.
func SdNotify(state string) (bool, error) {
	return SdNotifyFn(state)
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
