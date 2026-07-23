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
	"strings"

	slogenv "github.com/cbrewster/slog-env"
	"github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
)

var (
	configPath = flag.String("config", "", "path to base Jsonnet config file")
	listenAddr = flag.String("listen", ":8000", "address to listen and serve")
)

type deviceConfig struct {
	ApiKey   string `json:"api_key"`
	Location string `json:"location"`
	Name     string `json:"name,omitempty"`
	gauthbox.AuthboxConfig
}

// registerNativeFuncs registers 'url' as a native jsonnet function.
func registerNativeFuncs(vm *jsonnet.VM) {
	vm.NativeFunction(&jsonnet.NativeFunction{
		Name:   "url",
		Params: []ast.Identifier{"schemeHost", "path", "qs"},
		Func: func(args []interface{}) (interface{}, error) {
			schemeHost, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("argument 'schemeHost' must be a string")
			}
			pathSlice, ok := args[1].([]interface{})
			if !ok {
				return nil, fmt.Errorf("argument 'path' must be an array")
			}
			qsMap, ok := args[2].(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("argument 'qs' must be an object")
			}

			schemeHost = strings.TrimSuffix(schemeHost, "/")

			var paths []string
			for i, p := range pathSlice {
				s, ok := p.(string)
				if !ok {
					return nil, fmt.Errorf("path element at index %d must be a string", i)
				}
				paths = append(paths, url.PathEscape(s))
			}
			fullPath := strings.Join(paths, "/")
			if fullPath != "" {
				fullPath = "/" + fullPath
			}

			values := url.Values{}
			for k, v := range qsMap {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("query string value for key %q must be a string", k)
				}
				values.Set(k, s)
			}
			qStr := values.Encode()

			finalURL := schemeHost + fullPath
			if qStr != "" {
				finalURL += "?" + qStr
			}
			return finalURL, nil
		},
	})
}

// LoadConfig parses a Jsonnet configuration file, registers the custom native functions,
// and returns the parsed device configurations.
func LoadConfig(path string) (map[string]deviceConfig, error) {
	vm := jsonnet.MakeVM()
	registerNativeFuncs(vm)
	jsonStr, err := vm.EvaluateFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot evaluate Jsonnet config file: %w", err)
	}
	var devices map[string]deviceConfig
	if err := json.Unmarshal([]byte(jsonStr), &devices); err != nil {
		return nil, fmt.Errorf("cannot parse evaluated JSON: %w", err)
	}
	return devices, nil
}

func newConfigHandler(devices map[string]deviceConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		toolId := strings.TrimPrefix(r.URL.Path, "/config/")
		device, ok := devices[toolId]
		if !ok {
			slog.Error("tool not found", slog.String("toolId", toolId))
			w.WriteHeader(http.StatusNotFound)
			return
		}

		c := device.AuthboxConfig
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(c); err != nil {
			slog.Error("error encoding JSON response", slog.Any("err", err))
		}
		slog.Info("served authbox config", slog.String("toolId", toolId))
	}
}

func main() {
	slog.SetDefault(slog.New(slogenv.NewHandler(slog.NewTextHandler(os.Stderr, nil))))

	flag.Parse()
	if *configPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	devices, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	{
		ids := []string{}
		for toolId := range devices {
			ids = append(ids, toolId)
		}
		slog.Info("parsed authboxes from Jsonnet", slog.Any("toolIds", ids))
	}

	http.HandleFunc("/config/", newConfigHandler(devices))

	gauthbox.SdNotify("READY=1\n" + "STATUS=Listening on " + *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}
