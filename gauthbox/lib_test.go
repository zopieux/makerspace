package gauthbox

import (
	"sync"
	"testing"
	"time"
)

type testPwmController struct {
	mu         sync.Mutex
	duties     []float64
	dutyChan   chan float64
	closed     bool
}

func (t *testPwmController) SetDutyCycle(duty float64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.duties = append(t.duties, duty)
	select {
	case t.dutyChan <- duty:
	default:
	}
	return nil
}

func (t *testPwmController) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

type testGpioLine struct {
	mu     sync.Mutex
	values []int
	valCh  chan int
	closed bool
}

func (t *testGpioLine) SetValue(val int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.values = append(t.values, val)
	select {
	case t.valCh <- val:
	default:
	}
	return nil
}

func (t *testGpioLine) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

func TestTweenCurves(t *testing.T) {
	curves := []struct {
		name     string
		fn       TweenFunc
		t0       float64
		t05      float64
		t1       float64
	}{
		{"Linear", Linear, 0.0, 0.5, 1.0},
		{"EaseIn", EaseIn, 0.0, 0.125, 1.0},
		{"EaseOut", EaseOut, 0.0, 0.875, 1.0},
		{"EaseInOut", EaseInOut, 0.0, 0.5, 1.0},
	}

	const eps = 1e-9
	for _, tc := range curves {
		t.Run(tc.name, func(t *testing.T) {
			if v := tc.fn(0); v < tc.t0-eps || v > tc.t0+eps {
				t.Errorf("%s(0) = %f; want %f", tc.name, v, tc.t0)
			}
			if v := tc.fn(0.5); v < tc.t05-eps || v > tc.t05+eps {
				t.Errorf("%s(0.5) = %f; want %f", tc.name, v, tc.t05)
			}
			if v := tc.fn(1); v < tc.t1-eps || v > tc.t1+eps {
				t.Errorf("%s(1) = %f; want %f", tc.name, v, tc.t1)
			}
		})
	}
}

func TestPwmHelpers(t *testing.T) {
	hold := PwmHold(0.75, 5*time.Second)
	if hold.From != 0.75 || hold.To != 0.75 || hold.Duration != 5*time.Second {
		t.Errorf("Unexpected hold step: %+v", hold)
	}

	fadeDefault := PwmFade(1.0, 0.0, 2*time.Second)
	if fadeDefault.From != 1.0 || fadeDefault.To != 0.0 || fadeDefault.Duration != 2*time.Second {
		t.Errorf("Unexpected fade step: %+v", fadeDefault)
	}
	if fadeDefault.Curve(0.5) != 0.5 {
		t.Errorf("Expected default fade curve to be Linear, got %f", fadeDefault.Curve(0.5))
	}

	fadeEase := PwmFade(0.0, 1.0, 3*time.Second, EaseInOut)
	if fadeEase.Curve(0.25) != EaseInOut(0.25) {
		t.Errorf("Expected fade curve to match EaseInOut, got %f", fadeEase.Curve(0.25))
	}

	if len(NotAllowedAnimation.Steps) != 3 {
		t.Fatalf("Expected 3 steps in NotAllowedAnimation, got %d", len(NotAllowedAnimation.Steps))
	}
}

func TestBlinkerWithPwmAnimation(t *testing.T) {
	mockPwm := &testPwmController{dutyChan: make(chan float64, 100)}
	oldOpenPwm := OpenSysfsPwmFn
	OpenSysfsPwmFn = func(chip int, channel int, periodNs int, activeLow bool) (PwmController, error) {
		return mockPwm, nil
	}
	defer func() { OpenSysfsPwmFn = oldOpenPwm }()

	pwmChip := 0
	mode := make(chan LedMode, 10)
	cfg := LedConfig{
		Pin:     4,
		PwmChip: &pwmChip,
	}

	blinkerRunner, err := Blinker(cfg, "", mode)
	if err != nil {
		t.Fatalf("Failed to initialize Blinker: %v", err)
	}

	go blinkerRunner()

	// 1. Static ON
	mode <- LedStatic{On: true}
	select {
	case duty := <-mockPwm.dutyChan:
		if duty != 1.0 {
			t.Errorf("Expected duty 1.0 for LedStatic{On: true}, got %v", duty)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for static duty cycle update")
	}

	// 2. Animation sequence
	anim := LedAnimation{
		Steps: []PwmStep{
			PwmHold(1.0, 50*time.Millisecond),
			PwmFade(1.0, 0.0, 60*time.Millisecond),
			PwmFade(0.0, 1.0, 60*time.Millisecond),
		},
	}
	mode <- anim

	// Collect duty cycles during animation
	var seenDuties []float64
	timeout := time.After(250 * time.Millisecond)
collect:
	for {
		select {
		case d := <-mockPwm.dutyChan:
			seenDuties = append(seenDuties, d)
		case <-timeout:
			break collect
		}
	}

	if len(seenDuties) < 5 {
		t.Errorf("Expected multiple duty cycle updates during animation, got %d updates: %v", len(seenDuties), seenDuties)
	}

	// 3. Switch back to Static OFF
	mode <- LedStatic{On: false}
	select {
	case duty := <-mockPwm.dutyChan:
		if duty != 0.0 {
			t.Errorf("Expected duty 0.0 for LedStatic{On: false}, got %v", duty)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout waiting for static off duty cycle update")
	}

	close(mode)
}
