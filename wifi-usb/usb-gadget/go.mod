module usb-gadget

go 1.25.0

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/gvalkov/golang-evdev v0.0.0-20220815104727-7e27d6ce89b6
	github.com/semistrict/go-ublk v0.2.0
	github.com/warthog618/go-gpiocdev v0.9.1
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.36.0 // indirect
)

replace gauthbox => ../../gauthbox
