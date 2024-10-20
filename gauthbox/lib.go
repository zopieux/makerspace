package gauthbox

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	slogenv "github.com/cbrewster/slog-env"
	"github.com/holoplot/go-evdev"
	"github.com/warthog618/go-gpiocdev"
)

const BADGE_WANTED_VENDOR = 121
const BADGE_WANTED_PRODUCT = 6
const BADGE_TIMEOUT = 250 * time.Millisecond

const GPIO_WANTED_PREFIX = "pinctrl-bcm2"
const GPIO_DEBOUNCE = 100 * time.Millisecond

type badgeReaderConfig struct {
	Vendor int `json:"vendor"`
	Product int `json:"product"`
	Timeout int `json:"timeout_ms"`
}

type relayConfig struct {
	Debounce int `json:"debounce_ms"`
}

type RemoteConfig struct {
	MqttBroker string `json:"mqtt_broker,omitempty"`
	BadgeReader badgeReaderConfig `json:"badge_reader,omitempty"`
	Relay relayConfig `json:"relay,omitempty"`
}

func BadgeReader(c RemoteConfig, badging chan<- string) error {
	var err error
	device, err = findBadgeReader(c.BadgeReader)
	if err != nil {
		return err
	}
	if err := device.Grab(); err != nil {
		return err
	}
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
			timeout.Reset(c.BadgeReader.Timeout)
			switch e.Code {
			case evdev.KEY_LEFTSHIFT, evdev.KEY_RIGHTSHIFT:
				cap = true
			case evdev.KEY_ENTER:
				slog.Debug("badge: badged", slog.String("id", s))
				//pub("badged", s)
				badging <- s
				s = ""
				cap = false
			case key, ok := keyMap[e.Code]; ok:
				if cap {
					s += key.cap
				} else {
					s += key.normal
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

type gpio struct {
	chip  *gpiocdev.Chip
	lines []*gpiocdev.Line
}

func (b gpio) Name() string {
	return "gpio"
}

func parsePin(topic string) int {
	pinS := strings.Split(topic, "/")[0]
	pin, err := strconv.Atoi(pinS)
	if err != nil {
		return -1
	}
	return pin
}

func (b gpio) doLine(pin int, value int) {
	slog.Debug("gpio: request to set pin", slog.Int("pin", pin), slog.Int("value", value))
	l, err := b.chip.RequestLine(pin, gpiocdev.AsOutput(value))
	if err != nil {
		slog.Warn("gpio: could not set pin value: %s", err)
	} else {
		l.Close()
	}
}

func (b gpio) Topics() map[string]mqttHandle {
	return map[string]mqttHandle{
		"+/on": func(topic string, payload string, pub mqttPublish) {
			pin := parsePin(topic)
			if pin == -1 {
				return
			}
			b.doLine(pin, 1)
		},
		"+/off": func(topic string, payload string, pub mqttPublish) {
			pin := parsePin(topic)
			if pin == -1 {
				return
			}
			b.doLine(pin, 0)
		},
		"+/watch": func(topic string, payload string, pub mqttPublish) {
			pin := parsePin(topic)
			if pin == -1 {
				return
			}
			slog.Debug("gpio: request to watch pin", slog.Int("pin", pin))
			line, err := b.chip.RequestLine(pin,
				gpiocdev.AsInput,
				gpiocdev.WithPullUp,
				gpiocdev.WithBothEdges,
				gpiocdev.DebounceOption(GPIO_DEBOUNCE),
				gpiocdev.WithEventHandler(func(le gpiocdev.LineEvent) {
					value := "off"
					if le.Type == gpiocdev.LineEventRisingEdge {
						value = "on"
					}
					slog.Debug("gpip: pin transition", slog.Int("pin", pin), slog.String("value", value))
					pub(fmt.Sprintf("%d/level", pin), value)
				}))
			if err != nil {
				slog.Error("gpio: could not request pin", err)
				return
			}
			b.lines = append(b.lines, line)
		},
	}
}

func (b *gpio) Init(pub mqttPublish) error {
	var err error
	b.chip, err = findGpioChip()
	if err != nil {
		return err
	}
	b.lines = make([]*gpiocdev.Line, 0)
	return nil
}

// Finds the badge reader input device by vendor & product IDs.
func findBadgeReader(c badgeReaderConfig) (*evdev.InputDevice, error) {
	devicePaths, err := evdev.ListDevicePaths()
	if err != nil {
		return nil, err
	}
	for _, d := range devicePaths {
		device, err := evdev.Open(d.Path)
		if err != nil {
			return nil, err
		}
		inpId, err := device.InputID()
		if err != nil {
			return nil, err
		}
		if inpId.Vendor == c.Vendor && inpId.Product == c.Product {
			return device, nil
		}
	}
	return nil, errors.New("no badge reader found")
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
	return nil, errors.New("no such chip")
}

func main() {
	slog.SetDefault(slog.New(slogenv.NewHandler(slog.NewTextHandler(os.Stderr, nil))))

	mqttBroker := os.Args[1]
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("error getting hostname: %v", err)
	}

	topicPrefix := func(actor mqttActor) string {
		return fmt.Sprintf("%s/%s/", actor.Name(), hostname)
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(mqttBroker)
	opts.SetClientID("authbox/" + hostname)
	opts.SetConnectRetryInterval(2 * time.Second)

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		slog.Error("mqtt: lost connection", err)
		sdNotify("STATUS=mqtt: disconnected")
	})

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		doPublish := func(actor mqttActor) mqttPublish {
			prefix := topicPrefix(actor)
			return func(topic string, payload interface{}) {
				if t := c.Publish(prefix+topic, 0, false, payload); t.Wait() && t.Error() != nil {
					slog.Error("mqtt: error publishing", t.Error(), slog.String("topic", topic))
				}
			}
		}

		actors := []mqttActor{}
		for _, actor := range []mqttActor{&badge{}, &gpio{}} {
			if err := actor.Init(doPublish(actor)); err != nil {
				slog.Error("actor: error loading", err, slog.String("actor", actor.Name()))
			} else {
				slog.Info("actor: loaded successfully", slog.String("actor", actor.Name()))
				actors = append(actors, actor)
			}
		}

		for _, actor := range actors {
			prefix := topicPrefix(actor)
			for topic, handle := range actor.Topics() {
				if t := c.Subscribe(prefix+topic, 0, func(_ mqtt.Client, m mqtt.Message) {
					handle(strings.TrimPrefix(m.Topic(), prefix), string(m.Payload()), doPublish(actor))
				}); t.Wait() && t.Error() != nil {
					log.Fatalf("mqtt: fatal error subscribing: %v", t.Error())
				}
			}
		}
		sdNotify("READY=1")
		sdNotify(fmt.Sprintf("STATUS=mqtt: connected as '%s' to %s", hostname, mqttBroker))
		slog.Info("mqtt: connected", slog.String("broker", mqttBroker))
	})

	mc := mqtt.NewClient(opts)
	if t := mc.Connect(); t.Wait() && t.Error() != nil {
		log.Fatalf("mqtt: fatal error connecting: %v", t.Error())
	}

	for {
		time.Sleep(time.Minute)
	}
}

// sdNotify sends a message to systemd notify socket.
func sdNotify(state string) (bool, error) {
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

var keyMap := map[uint16]struct {
    normal string
    cap    string
}{
    evdev.KEY_SEMICOLON: {";", ":"},
    evdev.KEY_BACKSLASH: {"\\", "|"},
    evdev.KEY_SLASH:     {"/", "?"},
    evdev.KEY_COMMA:     {",", "<"},
    evdev.KEY_MINUS:     {"-", "_"},
    evdev.KEY_DOT:       {".", ">"},
    evdev.KEY_SPACE:     {" ", " "},
    evdev.KEY_GRAVE:      {"`", "~"},
    evdev.KEY_LEFTBRACE:  {"[", "{"},
    evdev.KEY_RIGHTBRACE: {"]", "}"},
    evdev.KEY_APOSTROPHE: {"'", "\""},
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
    evdev.KEY_EQUAL:      {"=", "+"},
}