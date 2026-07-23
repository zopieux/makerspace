package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-jsonnet"
)

func Test(t *testing.T) {
	jsonnetContent := `
local base = {
  mqtt: {
    broker: "tcp://127.0.0.1:1883",
    topic: "base_topic",
  },
  badge_reader: {
    name: "HID OMNIKEY",
  },
  badge_auth: {
    url_template: "http://auth/{{.tool}}?key={{.apiKey}}&name={{.name}}&loc={{.location}}",
  },
};

local api(key, name) = {
  api_key: key,
  name: name,
  location: "zrh",
};

{
  "device1": base + api("api1", "Device One") + {
    idle_duration_s: 30,
    green_led: { pin: 5 },
  },
  "device2": base + api("api2", "Device Two") + {
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

	var devices map[string]deviceConfig
	vm := jsonnet.MakeVM()
	jsonStr, err := vm.EvaluateAnonymousSnippet("config.jsonnet", jsonnetContent)
	if err != nil {
		t.Fatalf("failed to evaluate Jsonnet snippet: %v", err)
	}
	if err := json.Unmarshal([]byte(jsonStr), &devices); err != nil {
		t.Fatalf("failed to parse evaluated JSON: %v", err)
	}

	handler := newConfigHandler(devices)

	req1 := httptest.NewRequest(http.MethodGet, "/config/device1", nil)
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	resp1 := w1.Result()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK for device1, got %d", resp1.StatusCode)
	}

	body1, _ := io.ReadAll(resp1.Body)
	var res1 map[string]interface{}
	if err := json.Unmarshal(body1, &res1); err != nil {
		t.Fatalf("failed to parse response for device1: %v", err)
	}

	if int(res1["idle_duration_s"].(float64)) != 30 {
		t.Errorf("Expected idle_duration_s=30, got %v", res1["idle_duration_s"])
	}
	if res1["badge_auth"].(map[string]interface{})["url_template"] != "http://auth/device1?key=api1&name=Device%20One&loc=zrh" {
		t.Errorf("Unexpected url_template: %v", res1["badge_auth"].(map[string]interface{})["url_template"])
	}
	if _, hasGadget := res1["gadget"]; hasGadget {
		t.Errorf("device1 should not have 'gadget' field")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/config/device2", nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	resp2 := w2.Result()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK for device2, got %d", resp2.StatusCode)
	}

	body2, _ := io.ReadAll(resp2.Body)
	var res2 map[string]interface{}
	if err := json.Unmarshal(body2, &res2); err != nil {
		t.Fatalf("failed to parse response for device2: %v", err)
	}

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
