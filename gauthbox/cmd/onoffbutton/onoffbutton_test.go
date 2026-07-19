package main

import (
	"fmt"
	"gauthbox"
	"gauthbox/mqtttest"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockGpioLine struct {
	pin    int
	values chan int
}

func (m *mockGpioLine) SetValue(val int) error {
	select {
	case m.values <- val:
	default:
	}
	return nil
}

func (m *mockGpioLine) Close() error {
	return nil
}

type mockCloser struct{}

func (m *mockCloser) Close() error { return nil }

func TestOnOffButtonHarness(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("could not retrieve self hostname: %s", err)
	}
	haDeviceId := "authbox_" + hostname

	// 1. Start Mock MQTT Broker
	mqttBroker := mqtttest.StartMockMQTT(t)
	defer mqttBroker.Close()

	var authRequestsMutex sync.Mutex
	var authRequests []string

	// 2. Start HTTP Config / Auth Server
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Info("mockHTTP received request", slog.String("method", r.Method), slog.String("url", r.URL.String()))
		if strings.HasPrefix(r.URL.Path, "/config/") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"idle_duration_s": 2,
				"badge_reader": {
					"name": "HID OMNIKEY MOCK"
				},
				"badge_auth": {
					"url_template": "%s/auth?badge={{.badgeId}}&action={{.state}}",
					"usage_duration_minutes": 10
				},
				"green_led": {
					"pin": 10
				},
				"red_led": {
					"pin": 11
				},
				"current_sensing": {
					"pin": 12,
					"debounce_ms": 10
				},
				"relay": {
					"pin": 13
				},
				"on_button": {
					"pin": 14,
					"debounce_ms": 10
				},
				"off_button": {
					"pin": 15,
					"debounce_ms": 10
				},
				"mqtt": {
					"broker": "tcp://%s",
					"topic": "onoffbutton_topic"
				}
			}`, ts.URL, mqttBroker.Addr)
			return
		}
		if r.URL.Path == "/auth" {
			authRequestsMutex.Lock()
			authRequests = append(authRequests, r.URL.String())
			authRequestsMutex.Unlock()
			w.Header().Set("x-makerspace-username", "test_user")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// 3. Set up mock channels for GPIO pins
	greenCh := make(chan int, 1000)
	redCh := make(chan int, 1000)
	relayCh := make(chan int, 1000)

	var currentSensingCallback func(bool)
	var onButtonCallback func(bool)
	var offButtonCallback func(bool)

	// Override all low-level hardware hooks
	gauthbox.RequestOutputPinFn = func(pin int, initVal int) (gauthbox.GpioLine, error) {
		var ch chan int
		if pin == 10 {
			ch = greenCh
		} else if pin == 11 {
			ch = redCh
		} else if pin == 13 {
			ch = relayCh
		}
		return &mockGpioLine{pin: pin, values: ch}, nil
	}

	gauthbox.RequestInputPinFn = func(pin int, bias string, debounce time.Duration, callback func(bool)) (io.Closer, error) {
		if pin == 12 {
			currentSensingCallback = callback
		} else if pin == 14 {
			onButtonCallback = callback
		} else if pin == 15 {
			offButtonCallback = callback
		}
		return &mockCloser{}, nil
	}

	gauthbox.WriteSysfsLedFn = func(sysLedName string, brightness string) error {
		return nil
	}

	gauthbox.WriteSysfsLedTriggerFn = func(sysLedName string, trigger string) error {
		return nil
	}

	sdNotifyChan := make(chan string, 100)
	gauthbox.SdNotifyFn = func(state string) (bool, error) {
		select {
		case sdNotifyChan <- state:
		default:
		}
		return true, nil
	}

	// Mock evdev badge reader
	mockBadgeEvents := make(chan string, 10)
	gauthbox.BadgeReaderFn = func(c gauthbox.BadgeReaderConfig) (*gauthbox.DeviceRet[string], error) {
		return &gauthbox.DeviceRet[string]{
			Looper: func() {},
			Events: mockBadgeEvents,
			Mqtt: gauthbox.MqttComponent{
				Id: "badge",
				Component: func(baseTopic string) gauthbox.HaComponent {
					return gauthbox.HaComponent{
						Name:       "Badged in",
						Platform:   "sensor",
						BaseTopic:  baseTopic,
						StateTopic: "~/state",
					}
				},
				Publish: func(badgedIn interface{}) (string, interface{}) {
					return "/state", map[bool]string{false: "OFF", true: "ON"}[badgedIn.(bool)]
				},
			},
		}, nil
	}

	// 4. Override os.Args and start main in a goroutine
	os.Args = []string{"onoffbutton", ts.URL}
	go main()

	// Helper to assert MQTT topic/payload publications
	assertMQTTMessages := func(expected map[string]string, timeout time.Duration) {
		deadline := time.After(timeout)
		for len(expected) > 0 {
			select {
			case msg := <-mqttBroker.PubChan:
				if val, ok := expected[msg.Topic]; ok {
					if val == "*" || msg.Payload == val {
						delete(expected, msg.Topic)
					}
				}
			case <-deadline:
				t.Fatalf("Timeout waiting for expected MQTT messages. Remaining: %v", expected)
			}
		}
	}

	// Helper to check channel receives a value
	expectPinVal := func(ch chan int, expected int, desc string, timeout time.Duration) {
		deadline := time.After(timeout)
		for {
			select {
			case val := <-ch:
				if val == expected {
					return
				}
			case <-deadline:
				t.Fatalf("Timeout waiting for %s to be %d", desc, expected)
			}
		}
	}

	// Assert initial state:
	// - Relay is OFF (0)
	// - Red LED is ON (1)
	// - Green LED is OFF (0)
	expectPinVal(relayCh, 0, "relay OFF", time.Second)
	expectPinVal(redCh, 1, "red LED ON", time.Second)
	expectPinVal(greenCh, 0, "green LED OFF", time.Second)

	// Verify MQTT initial discovery and state
	assertMQTTMessages(expectedMQTTMsgMap(hostname), time.Second)

	// Verify Systemd ready notification
	select {
	case notify := <-sdNotifyChan:
		if notify != "READY=1" {
			t.Errorf("Expected READY=1, got '%s'", notify)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for READY=1 notification")
	}

	// 5. Badge In
	slog.Info("Simulating badge in...")
	mockBadgeEvents <- "998877"

	// Verify transition to STATE_IDLE:
	// - Relay remains OFF (0)
	// - Red LED turns OFF (0)
	// - Green LED starts blinking (should receive toggles: 0 then 1 then 0, or 1 then 0 then 1)
	select {
	case val := <-relayCh:
		if val != 0 {
			t.Fatalf("relay turned ON unexpectedly: %d", val)
		}
	case <-time.After(50 * time.Millisecond):
		// Correct, no relay write
	}
	expectPinVal(redCh, 0, "red LED OFF after badge", time.Second)
	expectPinVal(greenCh, 1, "green LED blinking transition", time.Second)

	// Verify MQTT pub for badged-in state
	assertMQTTMessages(map[string]string{
		"onoffbutton_topic/" + haDeviceId + "/badge/state": "ON",
	}, time.Second)

	// 6. Press ON Button
	slog.Info("Simulating ON button press...")
	onButtonCallback(true) // transitions to pressed

	// Verify Relay turns ON (1)
	expectPinVal(relayCh, 1, "relay ON after ON button press", time.Second)

	// 7. Fake power detection (Current sensing high)
	slog.Info("Simulating current sense high (power on)...")
	currentSensingCallback(true)

	// Green LED becomes solid ON (1)
	expectPinVal(greenCh, 1, "green LED solid ON", time.Second)

	// Verify MQTT current sensing state is "10" (drawing current)
	assertMQTTMessages(map[string]string{
		"onoffbutton_topic/" + haDeviceId + "/current/state": "10",
	}, time.Second)

	// 8. Press OFF Button
	slog.Info("Simulating OFF button press...")
	offButtonCallback(true) // transitions to pressed

	// Verify Relay turns OFF (0)
	expectPinVal(relayCh, 0, "relay OFF after OFF button press", time.Second)

	// Green LED starts blinking again
	expectPinVal(greenCh, 1, "green LED blinking again", time.Second)

	// Simulate current sense going low as consequence of relay turning off
	slog.Info("Simulating current sense low (power off)...")
	currentSensingCallback(false)

	// Verify MQTT current sensing state is "0"
	assertMQTTMessages(map[string]string{
		"onoffbutton_topic/" + haDeviceId + "/current/state": "0",
	}, time.Second)

	// 9. Wait for Idle Timeout (2 seconds)
	slog.Info("Waiting for idle timeout...")
	expectPinVal(relayCh, 0, "relay OFF after idle timeout", 3*time.Second)
	expectPinVal(greenCh, 0, "green LED OFF after idle timeout", time.Second)
	expectPinVal(redCh, 1, "red LED ON after idle timeout", time.Second)

	// Verify MQTT messages for detached / OFF state
	assertMQTTMessages(map[string]string{
		"onoffbutton_topic/" + haDeviceId + "/badge/state": "OFF",
	}, time.Second)

	// Wait a moment for background goroutines to complete their calls
	time.Sleep(100 * time.Millisecond)

	// Verify `/auth` calls
	authRequestsMutex.Lock()
	reqs := make([]string, len(authRequests))
	copy(reqs, authRequests)
	authRequestsMutex.Unlock()

	expectedAuth := []string{
		"/auth?badge=998877&action=initial",
		"/auth?badge=998877&action=return",
	}

	if len(reqs) != len(expectedAuth) {
		t.Errorf("Expected %d auth calls, got %d: %v", len(expectedAuth), len(reqs), reqs)
	} else {
		for i, expected := range expectedAuth {
			if !strings.HasSuffix(reqs[i], expected) {
				t.Errorf("Expected auth call suffix %q, got %q", expected, reqs[i])
			}
		}
	}
}

func expectedMQTTMsgMap(hostname string) map[string]string {
	haDeviceId := "authbox_" + hostname
	return map[string]string{
		"homeassistant/device/" + haDeviceId + "/config": "*",
		"onoffbutton_topic/" + haDeviceId + "/LWT":       "*",
	}
}
