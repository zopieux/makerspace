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
)

var (
	configPath = flag.String("config", "", "path to base JSON config file")
	listenAddr = flag.String("listen", ":8000", "address to listen and serve")
)

type authbox struct {
	ApiKey    string `json:"api_key"`
	Location  string `json:"location"`
	HumanName string `json:"name,omitempty"`
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
		if err := json.NewEncoder(w).Encode(c); err != nil {
			slog.Error("error encoding JSON response", slog.Any("err", err))
		}
		slog.Info("served authbox config", slog.String("toolId", toolId))
	})

	gauthbox.SdNotify("READY=1\n" + "STATUS=Listening on " + *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}
