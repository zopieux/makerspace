package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthboxConfig(t *testing.T) {
	jsonnetContent := `
local url(base, path, qs) = std.native("url")(base, path, qs);

local base(tool, apiKey, name, location) = {
  mqtt: {
    broker: "tcp://127.0.0.1:1883",
    topic: "base_topic",
  },
  badge_reader: {
    name: "HID OMNIKEY",
  },
  badge_auth: {
    url_template: url("http://auth", [tool], { key: apiKey, name: name, loc: location })
		+ "&badge={{.badgeId}}&state={{.state}}&duration={{.duration}}",
  },
};

{
  "device1": base("device1", "api1", "Device One", "zrh") + {
    idle_duration_s: 30,
    green_led: { pin: 5 },
  },
  "device2": base("device2", "api2", "Device Two", "zrh") + {
    on_button: { pin: 10, active_low: true, debounce_ms: 50, bias: "pull_up" },
    off_button: { pin: 11, active_low: false, debounce_ms: 20, bias: "pull_down" },
    gadget: {
      usb_label: "GADGET_USB",
      max_size: "100M",
      button: { pin: 5, active_low: true, debounce_ms: 10, bias: "pull_up" },
      status_led: { pin: 12, active_low: true },
    },
  },
}
`

	// Write jsonnet content to a temp file
	tmpDir, err := os.MkdirTemp("", "jsonnet-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configFilePath := filepath.Join(tmpDir, "config.jsonnet")
	if err := os.WriteFile(configFilePath, []byte(jsonnetContent), 0644); err != nil {
		t.Fatalf("failed to write temp config file: %v", err)
	}

	// Load configuration using the library's LoadConfig
	devices, err := LoadConfig(configFilePath)
	if err != nil {
		t.Fatalf("failed to LoadConfig: %v", err)
	}

	// Spin up an HTTP test server using the handler
	server := httptest.NewServer(newConfigHandler(devices))
	defer server.Close()

	// Helper to request a device config
	getDeviceConfig := func(toolId string) (map[string]interface{}, int) {
		resp, err := http.Get(server.URL + "/config/" + toolId)
		if err != nil {
			t.Fatalf("HTTP GET failed: %v", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, resp.StatusCode
		}

		var config map[string]interface{}
		if err := json.Unmarshal(body, &config); err != nil {
			t.Fatalf("failed to parse JSON response: %v", err)
		}
		return config, resp.StatusCode
	}

	// Test device1
	res1, code1 := getDeviceConfig("device1")
	if code1 != http.StatusOK {
		t.Errorf("Expected 200 OK for device1, got %d", code1)
	} else {
		if int(res1["idle_duration_s"].(float64)) != 30 {
			t.Errorf("Expected idle_duration_s=30, got %v", res1["idle_duration_s"])
		}
		expectedTemplate := "http://auth/device1?key=api1&loc=zrh&name=Device+One&badge={{.badgeId}}&state={{.state}}&duration={{.duration}}"
		if res1["badge_auth"].(map[string]interface{})["url_template"] != expectedTemplate {
			t.Errorf("Unexpected url_template: %v", res1["badge_auth"].(map[string]interface{})["url_template"])
		}
		if _, hasGadget := res1["gadget"]; hasGadget {
			t.Errorf("device1 should not have 'gadget' field")
		}
	}

	// Test device2
	res2, code2 := getDeviceConfig("device2")
	if code2 != http.StatusOK {
		t.Errorf("Expected 200 OK for device2, got %d", code2)
	} else {
		if res2["gadget"].(map[string]interface{})["usb_label"] != "GADGET_USB" {
			t.Errorf("Expected gadget.usb_label='GADGET_USB', got %v", res2["gadget"].(map[string]interface{})["usb_label"])
		}
		if btn := res2["on_button"].(map[string]interface{}); btn["pin"].(float64) != 10 || btn["bias"] != "pull_up" {
			t.Errorf("Expected on_button.pin=10 and bias='pull_up', got pin=%v bias=%v", btn["pin"], btn["bias"])
		}
		if btn := res2["off_button"].(map[string]interface{}); btn["pin"].(float64) != 11 || btn["bias"] != "pull_down" {
			t.Errorf("Expected off_button.pin=11 and bias='pull_down', got pin=%v bias=%v", btn["pin"], btn["bias"])
		}
		if gbtn := res2["gadget"].(map[string]interface{})["button"].(map[string]interface{}); gbtn["pin"].(float64) != 5 || gbtn["active_low"].(bool) != true {
			t.Errorf("Expected gadget.button pin=5 active_low=true, got %v", gbtn)
		}
		if sled := res2["gadget"].(map[string]interface{})["status_led"].(map[string]interface{}); sled["pin"].(float64) != 12 || sled["active_low"].(bool) != true {
			t.Errorf("Expected gadget.status_led pin=12 active_low=true, got %v", sled)
		}
		if _, hasIdle := res2["idle_duration_s"]; hasIdle {
			t.Errorf("device2 should not have 'idle_duration_s' field")
		}
	}

	// Test non-existing device
	_, code3 := getDeviceConfig("device_unknown")
	if code3 != http.StatusNotFound {
		t.Errorf("Expected 404 Not Found for unknown device, got %d", code3)
	}
}
