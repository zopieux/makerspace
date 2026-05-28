package main

import (
	"encoding/json"
	"flag"
	"gauthbox"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/template"

	slogenv "github.com/cbrewster/slog-env"
	"github.com/google/go-jsonnet"
)

var (
	configPath = flag.String("config", "", "path to base Jsonnet config file")
	listenAddr = flag.String("listen", ":8000", "address to listen and serve")
)

type deviceConfig struct {
	ApiKey      string `json:"api_key"`
	Location    string `json:"location"`
	Name        string `json:"name,omitempty"`
	gauthbox.AuthboxConfig
}

func main() {
	slog.SetDefault(slog.New(slogenv.NewHandler(slog.NewTextHandler(os.Stderr, nil))))

	flag.Parse()
	if *configPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	var devices map[string]deviceConfig
	{
		vm := jsonnet.MakeVM()
		jsonStr, err := vm.EvaluateFile(*configPath)
		if err != nil {
			log.Fatalf("cannot evaluate Jsonnet config file at %s: %s", *configPath, err)
		}
		if err := json.Unmarshal([]byte(jsonStr), &devices); err != nil {
			log.Fatalf("cannot parse evaluated JSON: %s", err)
		}
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
		if c.BadgeAuth != nil && c.BadgeAuth.UrlTemplate != "" {
			authUrlTemplate, err := template.New("url").Parse(c.BadgeAuth.UrlTemplate)
			if err != nil {
				slog.Error("error parsing auth URL template", slog.Any("err", err))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var authUrl strings.Builder
			if err := authUrlTemplate.Execute(&authUrl, map[string]string{
				"tool":     url.PathEscape(toolId),
				"apiKey":   url.PathEscape(device.ApiKey),
				"name":     url.PathEscape(device.Name),
				"location": url.PathEscape(device.Location),
			}); err != nil {
				slog.Error("error executing auth URL template", slog.Any("err", err))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			c.BadgeAuth.UrlTemplate = authUrl.String()
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(c); err != nil {
			slog.Error("error encoding JSON response", slog.Any("err", err))
		}
		slog.Info("served authbox config", slog.String("toolId", toolId))
	}
}
