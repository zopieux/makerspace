package main

import (
	"fmt"
	"gauthbox"
	"log"
	"log/slog"
	"os"
	"time"

	slogenv "github.com/cbrewster/slog-env"
)

const (
	STATE_OFF    = iota
	STATE_IDLE   = iota
	STATE_IN_USE = iota
)

type State struct {
	state   int
	badgeId string

	relay         bool
	mqttConnected bool
}

func main() {
	slog.SetDefault(slog.New(slogenv.NewHandler(slog.NewTextHandler(os.Stderr, nil))))

	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <control-command URL>\n", os.Args[0])
		os.Exit(1)
	}

	name, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	config, err := gauthbox.GetConfig(os.Args[1])
	if err != nil {
		panic(err)
	}
	slog.Info("got config", slog.Any("config", config))

	mqttDisco := []gauthbox.MqttDiscovery{}

	badgeDev, err := gauthbox.BadgeReader(config.BadgeReader)
	if err != nil {
		log.Fatalf("badge init: %s", err)
	}
	mqttDisco = append(mqttDisco, badgeDev.Discovery)
	go badgeDev.Looper()

	currentSenseDev, err := gauthbox.CurrentSensing(config.CurrentSensing)
	if err != nil {
		log.Fatalf("current sensing init: %s", err)
	}
	mqttDisco = append(mqttDisco, currentSenseDev.Discovery)
	go currentSenseDev.Looper()

	relay := make(chan bool)
	relayDev, err := gauthbox.Relay(config.Relay, relay)
	if err != nil {
		log.Fatalf("relay init: %s", err)
	}
	mqttDisco = append(mqttDisco, relayDev.Discovery)
	go relayDev.Looper()

	green := make(chan interface{})
	greenLed, err := gauthbox.Blinker(config.GreenLed, "ACT", green)
	if err != nil {
		log.Fatalf("green led init: %s", err)
	}
	go greenLed()

	red := make(chan interface{})
	redLed, err := gauthbox.Blinker(config.RedLed, "PWR", red)
	if err != nil {
		log.Fatalf("red led init: %s", err)
	}
	go redLed()

	var publish gauthbox.PublishFunc = func(s string, i interface{}) {}
	var mqttEvents <-chan gauthbox.MqttEvent
	if config.MqttBroker != nil {
		var mqttLooper func()
		mqttLooper, mqttEvents, publish = gauthbox.MqttBroker(name, *config.MqttBroker, mqttDisco)
		go mqttLooper()
	}

	idleDuration := time.Duration(config.IdleSeconds) * time.Second
	idleTimer := time.NewTimer(0)
	idleTimer.Stop()

	badgeExtendDuration := time.Duration(config.BadgeAuth.UsageMinutes) * time.Minute
	badgeExpired := time.NewTimer(0)
	badgeExpired.Stop()

	state := State{state: STATE_OFF, badgeId: "", relay: false, mqttConnected: false}

	setRelay := func(on bool) {
		state.relay = on
		relay <- on
		go relayDev.OnEvent(on, name, publish)
	}

	notifyState := func() {
		stateStr := state.String()
		slog.Debug("state changed", slog.String("state", stateStr))
		gauthbox.SdNotify("STATUS=" + stateStr)
	}

	setRelay(false)
	green <- gauthbox.LedStatic{On: false}
	red <- gauthbox.LedStatic{On: true}

	gauthbox.SdNotify("READY=1")
	notifyState()

	for {
		select {
		case e := <-mqttEvents:
			{
				if e.DisconnectedError == nil {
					state.mqttConnected = true
				} else {
					state.mqttConnected = false
				}
				go notifyState()
			}
		case badgeId := <-badgeDev.Events:
			go badgeDev.OnEvent(badgeId, name, publish)
			if state.state == STATE_IN_USE {
				continue
			}
			err := gauthbox.BadgeAuth(config.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_INITIAL)
			if err != nil {
				wasOff := state.state == STATE_OFF
				slog.Warn("error authenticating badge", slog.String("id", badgeId), slog.Any("error", err))
				red <- gauthbox.LedModeBlink{Interval: time.Millisecond * 120}
				time.Sleep(time.Millisecond * 1200)
				red <- gauthbox.LedStatic{On: wasOff}
			} else {
				state.state = STATE_IDLE
				state.badgeId = badgeId
				idleTimer.Reset(idleDuration)
				badgeExpired.Reset(badgeExtendDuration)
				green <- gauthbox.LedModeBlink{Interval: time.Millisecond * 500}
				red <- gauthbox.LedStatic{On: false}
				setRelay(true)
				go notifyState()
			}
		case currentIsHigh := <-currentSenseDev.Events:
			go currentSenseDev.OnEvent(currentIsHigh, name, publish)
			switch {
			case currentIsHigh:
				slog.Debug("current sensing is high")
				if state.state != STATE_IDLE {
					continue
				}
				idleTimer.Stop()
				state.state = STATE_IN_USE
				green <- gauthbox.LedStatic{On: true}
				go notifyState()
			case !currentIsHigh:
				slog.Debug("current sensing is low")
				if state.state != STATE_IN_USE {
					continue
				}
				state.state = STATE_IDLE
				idleTimer.Reset(idleDuration)
				green <- gauthbox.LedModeBlink{Interval: time.Millisecond * 500}
				go notifyState()
			}
		case <-badgeExpired.C:
			switch state.state {
			case STATE_IDLE:
			case STATE_IN_USE:
				go func(badgeId string) {
					err := gauthbox.BadgeAuth(config.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_EXTEND)
					if err != nil {
						// That extend call is only for informational purposes.
						// Do not cut off power if that fails. Stopping a machine while in use can be dangerous or expensive.
						slog.Warn("error authenticating badge for extend", slog.String("id", badgeId), slog.Any("error", err))
					}
				}(state.badgeId)
				badgeExpired.Reset(badgeExtendDuration)
			}
		case <-idleTimer.C:
			switch state.state {
			case STATE_IDLE:
				state.state = STATE_OFF
				setRelay(false)
				green <- gauthbox.LedStatic{On: false}
				red <- gauthbox.LedStatic{On: true}
				go func(badgeId string) {
					err := gauthbox.BadgeAuth(config.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_RETURN)
					if err != nil {
						// That return call is only for informational purposes.
						slog.Warn("error authenticating badge for return", slog.String("id", state.badgeId), slog.Any("error", err))
					}
				}(state.badgeId)
				state.badgeId = ""
				go notifyState()
			}
		}
	}
}

func (s State) String() string {
	badge := "n/a"
	if s.badgeId != "" {
		badge = s.badgeId
	}
	return fmt.Sprintf("state: %s, badged: %s, relay: %s, mqtt: %s",
		map[int]string{
			STATE_OFF:    "OFF (unauthenticated)",
			STATE_IDLE:   "IDLE (authenticated)",
			STATE_IN_USE: "IN USE (authenticated, drawing current)",
		}[s.state],
		badge,
		map[bool]string{false: "off", true: "on"}[s.relay],
		map[bool]string{false: "disconnected", true: "connected"}[s.mqttConnected])
}
