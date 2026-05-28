module usb-gadget

go 1.25.0

require (
	gauthbox v0.0.0-00010101000000-000000000000
	github.com/semistrict/go-ublk v0.2.0
	github.com/warthog618/go-gpiocdev v0.9.1
)

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/holoplot/go-evdev v0.0.0-20240306072622-217e18f17db1 // indirect
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
)

replace gauthbox => ../../gauthbox
