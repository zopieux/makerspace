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
	currentIsHigh bool
	mqttConnected bool
}

func main() {
	slog.SetDefault(slog.New(slogenv.NewHandler(slog.NewTextHandler(os.Stderr, nil))))

	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <control-command URL>\n", os.Args[0])
		os.Exit(1)
	}
	ccUrl := os.Args[1]

	name, err := os.Hostname()
	if err != nil {
		log.Fatalf("could not retrieve self hostname: %s", err)
	}

	config, err := gauthbox.GetConfig(ccUrl)
	if err != nil {
		log.Fatalf("could not retrieve or parse remote nor local config: %s", err)
	}
	slog.Info("got config", slog.Any("config", config))

	mqttComponents := []gauthbox.MqttComponent{}

	badgeDev, err := gauthbox.BadgeReader(config.BadgeReader)
	if err != nil {
		log.Fatalf("could not initialize badge reader: %s", err)
	}
	mqttComponents = append(mqttComponents, badgeDev.Mqtt)
	go badgeDev.Looper()

	currentSenseDev, err := gauthbox.CurrentSensing(config.CurrentSensing)
	if err != nil {
		log.Fatalf("could not initialize current sensing: %s", err)
	}
	mqttComponents = append(mqttComponents, currentSenseDev.Mqtt)
	go currentSenseDev.Looper()

	relay := make(chan bool)
	relayDev, err := gauthbox.Relay(config.Relay, relay)
	if err != nil {
		log.Fatalf("could not initialize relay: %s", err)
	}
	mqttComponents = append(mqttComponents, relayDev.Mqtt)
	go relayDev.Looper()

	green := make(chan interface{})
	greenLed, err := gauthbox.Blinker(config.GreenLed, "ACT", green)
	if err != nil {
		log.Fatalf("could not initialize green led: %s", err)
	}
	go greenLed()

	red := make(chan interface{})
	redLed, err := gauthbox.Blinker(config.RedLed, "PWR", red)
	if err != nil {
		log.Fatalf("could not initialize red led: %s", err)
	}
	go redLed()

	var mqttPublish gauthbox.PublishFunc = func(gauthbox.MqttComponent, interface{}) {}
	var mqttEvents <-chan gauthbox.MqttEvent
	if config.MqttBroker != nil {
		var mqttLooper func()
		mqttLooper, mqttEvents, mqttPublish = gauthbox.MqttBroker(name, *config.MqttBroker, mqttComponents)
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
		go mqttPublish(relayDev.Mqtt, on)
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
			// Nothing special, just report the state.
			// Not being able to communicate with MQTT is non-fatal.
			if e.DisconnectedError == nil {
				state.mqttConnected = true
				// Re-publish state so it's fresh.
				go mqttPublish(currentSenseDev.Mqtt, state.currentIsHigh)
				go mqttPublish(relayDev.Mqtt, state.relay)
				go mqttPublish(badgeDev.Mqtt, state.badgeId)
			} else {
				state.mqttConnected = false
			}
			go notifyState()
		case badgeId := <-badgeDev.Events:
			// Someone badged.
			if state.state == STATE_IN_USE {
				// If the tool is already in active use, nothing to do.
				continue
			}
			// Otherwise, the tool is either OFF or in grace period (IDLE).
			// Authenticate and switch the relay.
			err := gauthbox.BadgeAuth(config.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_INITIAL)
			if err != nil {
				// Blink the red LED a few times to provide “access denied” feedback.
				wasOff := state.state == STATE_OFF
				slog.Warn("error authenticating badge", slog.String("id", badgeId), slog.Any("error", err))
				red <- gauthbox.LedBlink{Interval: time.Millisecond * 120}
				time.Sleep(time.Millisecond * 1200)
				red <- gauthbox.LedStatic{On: wasOff}
			} else {
				// All good, power the machine and start IDLEing.
				state.state = STATE_IDLE
				state.badgeId = badgeId
				idleTimer.Reset(idleDuration)
				badgeExpired.Reset(badgeExtendDuration)
				green <- gauthbox.LedBlink{Interval: time.Millisecond * 500}
				red <- gauthbox.LedStatic{On: false}
				setRelay(true)
				go mqttPublish(badgeDev.Mqtt, state.badgeId)
				go notifyState()
			}
		case currentIsHigh := <-currentSenseDev.Events:
			// Current sensing went up or down.
			state.currentIsHigh = currentIsHigh
			go mqttPublish(currentSenseDev.Mqtt, state.currentIsHigh)
			switch {
			case currentIsHigh:
				if state.state != STATE_IDLE {
					// Not supposed to happen, but anyway, bail.
					continue
				}
				// The machine is now in use, inhibit the idle timer.
				idleTimer.Stop()
				state.state = STATE_IN_USE
				green <- gauthbox.LedStatic{On: true}
				go notifyState()
			case !currentIsHigh:
				if state.state != STATE_IN_USE {
					// Not supposed to happen, but anyway, bail.
					continue
				}
				// The machine stopped drawing current. Start the idle timer in preparation of shutting off.
				state.state = STATE_IDLE
				idleTimer.Reset(idleDuration)
				green <- gauthbox.LedBlink{Interval: time.Millisecond * 500}
				go notifyState()
			}
		case <-badgeExpired.C:
			// The badge authentication duration (e.g. 10 minutes) has expired.
			if state.state == STATE_OFF {
				continue
			}
			badgeExpired.Reset(badgeExtendDuration)
			// Authenticate again in the background if the machine is not OFF.
			// This is only to accurately keep track of the real utilization duration.
			go func(badgeId string) {
				err := gauthbox.BadgeAuth(config.BadgeAuth, badgeId, gauthbox.BADGE_ACTION_EXTEND)
				if err != nil {
					// That extend call is only for informational purposes.
					// Do not cut off power if that fails. Stopping a machine while in use can be dangerous or expensive.
					slog.Warn("error authenticating badge for extend", slog.String("id", badgeId), slog.Any("error", err))
				}
			}(state.badgeId)
		case <-idleTimer.C:
			// The machine is not drawing current and we've reach the idle timeout.
			// Turn the power relay off, de-authenticate and return unused minutes.
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
				go mqttPublish(badgeDev.Mqtt, state.badgeId)
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
