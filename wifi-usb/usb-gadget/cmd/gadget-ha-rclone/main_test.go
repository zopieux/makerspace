package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gauthbox"
	"gauthbox/mqtttest"
	"usb-gadget/usb"
)

const (
	mqttPacketConnect   = 1
	mqttPacketSubscribe = 8
	mqttPacketPingreq   = 12
)

type mockUblkDevice struct{}

func (m *mockUblkDevice) Close() error {
	return nil
}

func (m *mockUblkDevice) DevPath() string {
	return "/dev/mock-ublk"
}

type mockRcloneSyncer struct {
	Files map[string]string
}

func (m *mockRcloneSyncer) Sync(rcloneConfigPath, source, portalDir string, extraConfig gauthbox.UsbGadgetConfig) ([]byte, error) {
	fsCtrl.MkdirAll(portalDir, 0755)
	for name, content := range m.Files {
		err := fsCtrl.WriteFile(filepath.Join(portalDir, name), []byte(content), 0644)
		if err != nil {
			return nil, err
		}
	}
	return []byte("success"), nil
}

type mockDirEntry struct {
	name string
}

func (m mockDirEntry) Name() string { return m.name }
func (m mockDirEntry) IsDir() bool { return false }
func (m mockDirEntry) Type() os.FileMode { return 0 }
func (m mockDirEntry) Info() (os.FileInfo, error) { return nil, nil }

type mockFileSystem struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMockFileSystem() *mockFileSystem {
	return &mockFileSystem{files: make(map[string][]byte)}
}

func (m *mockFileSystem) RemoveAll(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.files {
		if strings.HasPrefix(k, path) {
			delete(m.files, k)
		}
	}
	return nil
}

func (m *mockFileSystem) MkdirAll(path string, perm os.FileMode) error {
	return nil
}

func (m *mockFileSystem) WriteFile(filename string, data []byte, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[filename] = data
	return nil
}

func (m *mockFileSystem) ReadDir(dirname string) ([]os.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var entries []os.DirEntry
	seen := make(map[string]bool)
	for k := range m.files {
		if strings.HasPrefix(k, dirname) {
			rel := strings.TrimPrefix(k, dirname)
			rel = strings.TrimPrefix(rel, "/")
			parts := strings.Split(rel, "/")
			if len(parts) > 0 && parts[0] != "" {
				if !seen[parts[0]] {
					seen[parts[0]] = true
					entries = append(entries, mockDirEntry{name: parts[0]})
				}
			}
		}
	}
	return entries, nil
}

type mockOrchestrator struct {
	mu         sync.Mutex
	isBound    bool
	lastMode   usb.USBMode
	initCalled bool
	modeChan   chan usb.USBMode
	ublkChan   chan struct{}
}

func newMockOrchestrator() *mockOrchestrator {
	return &mockOrchestrator{
		modeChan: make(chan usb.USBMode, 10),
		ublkChan: make(chan struct{}, 10),
	}
}

func (m *mockOrchestrator) SwitchUSBMode(mode usb.USBMode) error {
	m.mu.Lock()
	m.lastMode = mode
	m.mu.Unlock()
	m.modeChan <- mode
	return nil
}

func (m *mockOrchestrator) GadgetInit(cfg usb.GadgetConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initCalled = true
	return nil
}

func (m *mockOrchestrator) GadgetClose() {}

func (m *mockOrchestrator) IsBound() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isBound
}

func (m *mockOrchestrator) Detach() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isBound = false
}

func (m *mockOrchestrator) UblkUpdate(sourceDir, label string) (usb.UblkDevice, error) {
	m.mu.Lock()
	m.isBound = true
	m.mu.Unlock()
	select {
	case m.ublkChan <- struct{}{}:
	default:
	}
	return &mockUblkDevice{}, nil
}

func Test(t *testing.T) {
	// 1. Start the Mock MQTT Broker
	mqttBroker := mqtttest.StartMockMQTT(t)
	defer mqttBroker.Close()

	// 2. Start the Mock HTTP C&C Config and Auth Server
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/config/") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"mqtt": {
					"broker": "%s",
					"topic": "onefinity_cnc",
					"device_id": "test_cnc",
					"device_name": "Test CNC"
				},
				"badge_reader": {
					"name": "HID OMNIKEY MOCK",
					"timeout_ms": 250
				},
				"badge_auth": {
					"url_template": "%s/auth?badge={{.badgeId}}&state={{.state}}"
				},
				"gadget": {
					"usb_label": "TEST_USB"
				}
			}`, "tcp://"+mqttBroker.Addr, ts.URL)
			return
		}
		if r.URL.Path == "/auth" {
			w.Header().Set("x-makerspace-username", "test_user")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Override global configUrl to point to our mock server
	configUrl = ts.URL + "/config/onefinity-cnc"

	// Mock File System (in-memory)
	mockFs := newMockFileSystem()
	fsCtrl = mockFs

	// Mock Rclone Syncer
	mockSyncer := &mockRcloneSyncer{
		Files: map[string]string{
			"box.nc":     "G0 X10 Y10\nG1 Z-1 F100\n",
		},
	}
	rcloneCtrl = mockSyncer

	// 3. Setup Mock Orchestrator
	mockOrch := newMockOrchestrator()
	usbCtrl = mockOrch

	// 4. Setup Mock Evdev Badge Reader Hook in gauthbox
	badgeChan := make(chan string, 10)
	gauthbox.BadgeReaderFn = func(c gauthbox.BadgeReaderConfig) (*gauthbox.DeviceRet[string], error) {
		return &gauthbox.DeviceRet[string]{
			Looper: func() {}, // Empty looper, events are simulated directly via badgeChan
			Events: badgeChan,
		}, nil
	}

	// 5. Run the main() in a background goroutine!
	slog.Info("Starting main() implementation...")
	go main()

	// 6. Verify Startup Phase
	// Wait for the host startup transition (Host mode switch)
	select {
	case mode := <-mockOrch.modeChan:
		if mode != usb.USBModeHost {
			t.Errorf("Expected startup mode switch to Host, got %s", mode)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for startup mode switch to Host")
	}

	// 7. Verify MQTT Connection & Discovery configurations
	// The client should send CONNECT (1) and SUBSCRIBE (8) packets to our mock MQTT broker
	hasConnect := false
	hasSubscribe := false
	deadline := time.After(time.Second)

	for !hasConnect || !hasSubscribe {
		select {
		case packet := <-mqttBroker.Packets:
			if packet == mqttPacketConnect {
				hasConnect = true
			} else if packet == mqttPacketSubscribe {
				hasSubscribe = true
			}
		case <-deadline:
			t.Fatalf("Timeout waiting for MQTT Connect/Subscribe packets. Connect: %v, Subscribe: %v", hasConnect, hasSubscribe)
		}
	}

	// Helper to assert a set of expected MQTT topic/payload publications
	assertMQTTMessages := func(t *testing.T, expected map[string]string, timeout time.Duration) {
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

	// Verify MQTT startup publications (Discovery config, LWT online, and detached status)
	assertMQTTMessages(t, map[string]string{
		"homeassistant/device/authbox_test_cnc/config": "*",
		"onefinity_cnc/authbox_test_cnc/LWT":          "online",
		"onefinity_cnc/authbox_test_cnc/status/state": "detached",
	}, time.Second)

	// 8. Simulate a badge scan event!
	slog.Info("Simulating badge scan event '887766'...")
	badgeChan <- "887766"

	// Wait for the orchestrator to switch mode to Peripheral
	select {
	case mode := <-mockOrch.modeChan:
		if mode != usb.USBModePeripheral {
			t.Errorf("Expected transition to Peripheral mode after badging, got %s", mode)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for transition to Peripheral mode")
	}

	// Wait for UblkUpdate to complete
	select {
	case <-mockOrch.ublkChan:
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for UblkUpdate to complete")
	}

	// Barrier to wait for the worker queue to finish processing the doAttach function
	doneChan := make(chan struct{})
	cmdQueue <- func() {
		close(doneChan)
	}
	select {
	case <-doneChan:
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for attach task barrier")
	}

	// Verify that the orchestrator is bound and initialization was triggered
	mockOrch.mu.Lock()
	bound := mockOrch.isBound
	initCalled := mockOrch.initCalled
	mockOrch.mu.Unlock()

	if !bound {
		t.Error("Expected Orchestrator to be bound after successful badge authentication")
	}
	if !initCalled {
		t.Error("Expected GadgetInit to have been triggered")
	}

	// Verify mock files are present in the portal directory
	portalDir := "/dev/shm/usb-gadget"
	entries, err := fsCtrl.ReadDir(portalDir)
	if err != nil {
		t.Fatalf("Failed to read portal directory: %v", err)
	}
	fileMap := make(map[string]bool)
	for _, entry := range entries {
		fileMap[entry.Name()] = true
	}
	if !fileMap["box.nc"] || !fileMap["[ logged in as test_user ].txt"] {
		t.Errorf("Expected mock files 'box.nc' and '[ logged in as test_user ].txt' in portalDir, got %v", fileMap)
	}

	// Verify published messages for attach flow
	assertMQTTMessages(t, map[string]string{
		"onefinity_cnc/authbox_test_cnc/status/state":   "attached",
		"onefinity_cnc/authbox_test_cnc/username/state": "test_user",
	}, time.Second)

	// 9. Simulate a detach request via MQTT command topic!
	slog.Info("Simulating MQTT detach command...")
	mqttBroker.PublishToClient("onefinity_cnc/authbox_test_cnc/detach/cmd", "PRESS")

	// Barrier to wait for the worker queue to finish processing the doDetach function
	doneChan2 := make(chan struct{})
	cmdQueue <- func() {
		close(doneChan2)
	}
	select {
	case <-doneChan2:
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for detach task barrier")
	}

	// Wait for the orchestrator to switch back to Host mode
	select {
	case mode := <-mockOrch.modeChan:
		if mode != usb.USBModeHost {
			t.Errorf("Expected mode switch back to Host after detach, got %s", mode)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for Host mode switch after detach")
	}

	mockOrch.mu.Lock()
	boundAfter := mockOrch.isBound
	mockOrch.mu.Unlock()

	if boundAfter {
		t.Error("Expected Orchestrator to be detached/unbound after detach request")
	}

	// Verify published messages for detach flow
	assertMQTTMessages(t, map[string]string{
		"onefinity_cnc/authbox_test_cnc/status/state":   "detached",
		"onefinity_cnc/authbox_test_cnc/username/state": "",
	}, time.Second)

	// 10. Verify Extra unmarshal works
	if conf.UsbLabel != "TEST_USB" {
		t.Errorf("Expected UsbLabel 'TEST_USB', got '%s'", conf.UsbLabel)
	}
}
