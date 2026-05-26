#!/usr/bin/env nix-shell
#!nix-shell -i bash -p python3

python3 -c '
import http.server
import json
import sys

myip = "192.168.1.30"
port = 8000

if len(sys.argv) > 1:
    try:
        port = int(sys.argv[1])
    except ValueError:
        print(f"Invalid port: {sys.argv[1]}. Using default port 8000.")

class ConfigHandler(http.server.BaseHTTPRequestHandler):
    def send_auth(self):
        self.send_response(200)
        self.send_header("x-makerspace-username", "example")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(b"OK\n")

    def do_GET(self):
        if self.path == "/config/onefinity-cnc":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            config = {
                "auth_url_template": f"http://{myip}:{port}/auth",
                "mqtt_broker": f"tcp://{myip}:1883",
                "mqtt_client_id": "onefinity_cnc",
                "mqtt_base_topic": "onefinity_cnc",
                "mqtt_device_id": "onefinity_cnc",
                "mqtt_device_name": "Onefinity CNC",
                "rclone_prefix": "drive:Dirname",
                "max_transfer": "64M",
                "max_size": "10M",
                "max_age": "365d",
                "ha_sensor_model": "Onefinity CNC USB Storage",
                "ha_sensor_manufacturer": "ZRH Woodshop",
                "usb_label": "WOODZRH",
                "usb_serial_number": "0123456789",
                "usb_manufacturer": "WOODZRH",
                "usb_product": "DropPortal",
                "usb_config_name": "USB Storage",
                "usb_inquiry_vendor": "WOODZRH",
                "usb_inquiry_product": "DropPortal"
            }
            self.wfile.write(json.dumps(config, indent=2).encode("utf-8"))
        elif self.path == "/auth":
            self.send_auth()
        else:
            self.send_response(404)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(b"Not Found\n")

    def do_POST(self):
        if self.path == "/auth":
            self.send_auth()
        else:
            self.send_response(404)
            self.end_headers()



print(f"Starting dev config server on port {port}...")
try:
    http.server.HTTPServer(("", port), ConfigHandler).serve_forever()
except KeyboardInterrupt:
    print("\nShutting down config server.")
    sys.exit(0)
' "$@"

