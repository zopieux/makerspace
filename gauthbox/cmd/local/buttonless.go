package main

import (
	"fmt"
	"gauthbox"
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

	name, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	config, err := gauthbox.GetConfig()
	if err != nil {
		panic(err)
	}
	slog.Info("received config", slog.Any("config", config))

	// if config.BadgeReader == nil {
	// 	panic("buttonless needs a BadgeReader config")
	// }
	// if config.CurrentSensing == nil {
	// 	panic("buttonless needs a CurrentSensing config")
	// }
	// if config.Relay == nil {
	// 	panic("buttonless needs a Relay config")
	// }
	// if config.GreenLed == nil {
	// 	panic("buttonless needs a green led config")
	// }
	// if config.RedLed == nil {
	// 	panic("buttonless needs a red led config")
	// }

	var mqttDisco []gauthbox.MqttDiscovery

	badgeDev, err := gauthbox.BadgeReader(name, config.BadgeReader)
	if badgeDev != nil {
		panic(err)
	}
	mqttDisco = append(mqttDisco, badgeDev.Discovery)
	go badgeDev.Looper()

	currentSenseDev, err := gauthbox.CurrentSensing(name, config.CurrentSensing)
	if err != nil {
		panic(err)
	}
	mqttDisco = append(mqttDisco, currentSenseDev.Discovery)
	go currentSenseDev.Looper()

	relay := make(chan bool)
	relayDev, err := gauthbox.Relay(name, config.Relay, relay)
	if err != nil {
		panic(err)
	}
	mqttDisco = append(mqttDisco, relayDev.Discovery)
	go relayDev.Looper()

	green := make(chan interface{})
	greenLed, err := gauthbox.Blinker(config.GreenLed, green)
	if err != nil {
		panic(err)
	}
	go greenLed()

	red := make(chan interface{})
	redLed, err := gauthbox.Blinker(config.RedLed, red)
	if err != nil {
		panic(err)
	}
	go redLed()

	var publish gauthbox.PublishFunc
	var mqttEvents <-chan gauthbox.MqttEvent
	if config.MqttBroker != nil {
		mqttEvents, publish, err = gauthbox.MqttBroker(name, *config.MqttBroker, mqttDisco)
		if err != nil {
			panic(err)
		}
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
		go relayDev.OnEvent(on, publish)
	}

	notifyState := func() {
		gauthbox.SdNotify("STATUS=" + state.String())
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
			go badgeDev.OnEvent(badgeId, publish)
			if state.state == STATE_IN_USE {
				return
			}
			err := gauthbox.BadgeAuth(config.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_INITIAL)
			if err != nil {
				slog.Warn("error authenticating badge", slog.String("id", badgeId), slog.Any("error", err))
				red <- gauthbox.LedModeBlink{Interval: time.Millisecond * 250}
				time.Sleep(time.Millisecond * 1500)
				red <- gauthbox.LedStatic{On: state.state == STATE_OFF}
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
			go currentSenseDev.OnEvent(currentIsHigh, publish)
			switch {
			case currentIsHigh:
				slog.Info("current sensing is high")
				if state.state != STATE_IDLE {
					return
				}
				idleTimer.Stop()
				state.state = STATE_IN_USE
				green <- gauthbox.LedStatic{On: true}
				go notifyState()
			case !currentIsHigh:
				slog.Info("current sensing is low")
				if state.state != STATE_IN_USE {
					return
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
	return fmt.Sprintf("state: %s, badged: %s, relay: %s, mqtt: %s",
		map[int]string{
			STATE_OFF:    "off (unauthenticated)",
			STATE_IDLE:   "idle (authenticated, not drawing current)",
			STATE_IN_USE: "in use (authenticated, drawing current)",
		}[s.state],
		s.badgeId,
		map[bool]string{false: "off", true: "on"}[s.relay],
		map[bool]string{false: "disconnected", true: "connected"}[s.mqttConnected])
}
