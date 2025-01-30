package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"gauthbox"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"text/template"

	slogenv "github.com/cbrewster/slog-env"
)

var (
	configPath = flag.String("config", "", "path to base JSON config file")
	listenAddr = flag.String("listen", ":8000", "address to listen and serve")
)

type authbox struct {
	ApiKey       string                 `json:"api_key"`
	Location     string                 `json:"location"`
	HumanName    string                 `json:"name,omitempty"`
	CustomConfig map[string]interface{} `json:"custom,omitempty"`
}

type baseConfig struct {
	AuthboxConfig gauthbox.AuthboxConfig `json:"config"`
	Authboxes     map[string]authbox     `json:"authboxes"`
}

func main() {
	slog.SetDefault(slog.New(slogenv.NewHandler(slog.NewTextHandler(os.Stderr, nil))))

	flag.Parse()
	if *configPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	var config baseConfig
	{
		f, err := os.Open(*configPath)
		if err != nil {
			log.Fatalf("cannot open config file at %s: %s", *configPath, err)
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&config); err != nil {
			log.Fatalf("cannot parse JSON config file at %s: %s", *configPath, err)
		}
	}

	{
		ids := []string{}
		for toolId := range config.Authboxes {
			ids = append(ids, toolId)
		}
		slog.Info("parsed authboxes", slog.Any("toolIds", ids))
	}

	authUrlTemplate, err := template.New("url").Parse(config.AuthboxConfig.BadgeAuth.UrlTemplate)
	if err != nil {
		log.Fatalf("config.badge_auth.url_template is not a valid Go template: %s", config.AuthboxConfig.BadgeAuth.UrlTemplate)
	}

	http.HandleFunc("/config/", func(w http.ResponseWriter, r *http.Request) {
		toolId := strings.TrimPrefix(r.URL.Path, "/config/")
		device, ok := config.Authboxes[toolId]
		if !ok {
			slog.Error("tool not found", slog.String("toolId", toolId))
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var authUrl strings.Builder
		if err := authUrlTemplate.Execute(&authUrl, map[string]string{
			"tool":     url.PathEscape(toolId),
			"apiKey":   url.PathEscape(device.ApiKey),
			"name":     url.PathEscape(device.HumanName),
			"location": url.PathEscape(device.Location),
		}); err != nil {
			slog.Error("error executing auth URL template", slog.Any("err", err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c := gauthbox.AuthboxConfig(config.AuthboxConfig)
		c.BadgeAuth.UrlTemplate = authUrl.String()
		// Apply per-tool customization, if any.
		for key, val := range device.CustomConfig {
			if err := setByPath(&c, val, strings.Split(key, ".")...); err != nil {
				slog.Warn("could not set custom config key to desired value",
					slog.String("key", key), slog.Any("value", val), slog.Any("error", err))
			}
		}
		if err := json.NewEncoder(w).Encode(c); err != nil {
			slog.Error("error encoding JSON response", slog.Any("err", err))
		}
		slog.Info("served authbox config", slog.String("toolId", toolId))
	})

	gauthbox.SdNotify("READY=1\n" + "STATUS=Listening on " + *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

// setByPath sets a nested field of an object using a path slice, handling integer types, json tag names
func setByPath(obj interface{}, value interface{}, path ...string) error {
	val := reflect.ValueOf(obj)
	if val.Kind() != reflect.Ptr || val.IsNil() {
		return fmt.Errorf("object must be a non-nil pointer")
	}
	val = val.Elem() // Get value pointed to

	for i, part := range path {
		if val.Kind() == reflect.Ptr && val.IsNil() {
			val.Set(reflect.New(val.Type().Elem()))
			val = val.Elem()
		}

		if val.Kind() != reflect.Struct && val.Kind() != reflect.Slice {
			return fmt.Errorf("cannot descend into non-struct/slice value at '%s' in '%s'", part, strings.Join(path[:i], "."))
		}

		if val.Kind() == reflect.Struct {
			field, ok := findFieldByJSONTag(val, part)
			if !ok {
				return fmt.Errorf("invalid field name '%s' in path element '%s' on object of kind '%s'", part, strings.Join(path[:i], "."), val.Type())
			}
			val = field
		} else if val.Kind() == reflect.Slice {
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 {
				return fmt.Errorf("invalid index '%s' in path element '%s' on slice '%s' ", part, strings.Join(path[:i], "."), val.Type())
			}

			if index >= val.Len() {
				if i != len(path)-1 {
					return fmt.Errorf("index '%d' out of slice '%s' bounds at '%s'", index, val.Type(), strings.Join(path[:i+1], "."))
				}

				newSlice := reflect.MakeSlice(val.Type(), index+1, index+1)
				reflect.Copy(newSlice, val)
				val.Set(newSlice)

				if val.Index(index).Kind() == reflect.Ptr {
					val.Index(index).Set(reflect.New(val.Type().Elem().Elem()))
				} else if val.Index(index).Kind() == reflect.Struct {
					val.Index(index).Set(reflect.New(val.Type().Elem()).Elem())
				}

			}
			val = val.Index(index)
		}
	}

	if !val.CanSet() {
		return fmt.Errorf("cannot set '%s' on '%s'", strings.Join(path, "."), reflect.ValueOf(obj).Type())
	}

	v := reflect.ValueOf(value)

	switch val.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		intVal, ok := convertToInt64(v)
		if !ok {
			return fmt.Errorf("value of type '%s' cannot be assigned to int-like field of type '%s'", v.Type(), val.Type())
		}
		if val.OverflowInt(intVal) {
			return fmt.Errorf("value of type '%s' with value '%d' cannot be assigned to field of type '%s' due to overflow", v.Type(), intVal, val.Type())
		}
		val.SetInt(intVal)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		uintVal, ok := convertToUint64(v)
		if !ok {
			return fmt.Errorf("value of type '%s' cannot be assigned to uint-like field of type '%s'", v.Type(), val.Type())
		}
		if val.OverflowUint(uintVal) {
			return fmt.Errorf("value of type '%s' with value '%d' cannot be assigned to field of type '%s' due to overflow", v.Type(), uintVal, val.Type())
		}

		val.SetUint(uintVal)
	default:
		if !v.Type().AssignableTo(val.Type()) {
			return fmt.Errorf("value of type '%s' cannot be assigned to field of type '%s'", v.Type(), val.Type())
		}
		val.Set(v)
	}

	return nil
}

func findFieldByJSONTag(val reflect.Value, jsonTag string) (reflect.Value, bool) {
	for i := 0; i < val.NumField(); i++ {
		field := val.Type().Field(i)
		tag := field.Tag.Get("json")
		if tag == jsonTag {
			return val.Field(i), true
		}
		if tag == "" && field.Name == jsonTag {
			return val.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func convertToInt64(v reflect.Value) (int64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return int64(v.Float()), true
	default:
		return 0, false
	}
}

func convertToUint64(v reflect.Value) (uint64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint(), true
	case reflect.Float32, reflect.Float64:
		return uint64(v.Float()), true
	default:
		return 0, false
	}
}
