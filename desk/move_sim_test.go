package desk

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// simDesk is a DPG controller reduced to the rules example/moves has
// measured, on a faster clock:
//
//   - at rest, it takes a height only if it has heard nothing for simQuiet;
//   - moving, the same height sustains it and a different one halts it,
//     whichever way that height lies (the extend scenario);
//   - moving, silence for simCoast lets it run down to rest;
//   - it reports while it moves, and once with speed 0 when it stops.
//
// It counts halts, which is what the tests below are about: Move must not
// cost the user a halt it could have avoided.
type simDesk struct {
	mu        sync.Mutex
	pos       int
	target    int
	moving    bool
	lastWrite time.Time
	halts     int
	rests     []int // where it came to rest, in order
	readings  *slot[reading]
	stop      chan struct{}
}

const (
	simStep  = 40 // tenths of a millimetre per tick
	simTick  = 10 * time.Millisecond
	simQuiet = 150 * time.Millisecond
	simCoast = 150 * time.Millisecond
)

func newSimDesk(pos int, readings *slot[reading]) *simDesk {
	s := &simDesk{pos: pos, readings: readings, stop: make(chan struct{})}
	go s.run()
	return s
}

func (s *simDesk) WriteWithoutResponse(data []byte) (int, error) {
	h := int(binary.LittleEndian.Uint16(data))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	quiet := s.lastWrite.IsZero() || now.Sub(s.lastWrite) >= simQuiet
	s.lastWrite = now
	switch {
	case s.moving && h == s.target:
		// sustained
	case s.moving:
		s.halts++
		s.restLocked()
	case quiet && abs(h-s.pos) > arrivalTolerance:
		s.target, s.moving = h, true
	}
	return len(data), nil
}

func (s *simDesk) restLocked() {
	s.moving = false
	s.rests = append(s.rests, s.pos)
	s.readings.put(reading{extension: s.pos, speed: 0})
}

func (s *simDesk) run() {
	t := time.NewTicker(simTick)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		if s.moving {
			if time.Since(s.lastWrite) >= simCoast {
				s.restLocked()
			} else {
				dir := sign(s.target - s.pos)
				if abs(s.target-s.pos) <= simStep {
					s.pos = s.target
					s.restLocked()
				} else {
					s.pos += dir * simStep
					s.readings.put(reading{extension: s.pos, speed: dir * 6224})
				}
			}
		}
		s.mu.Unlock()
	}
}

func (s *simDesk) state() (pos, halts int, rests []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pos, s.halts, append([]int(nil), s.rests...)
}

// simMove runs a move against a simulated desk, sending each of later at
// its offset from the start, and returns once the move is over.
func simMove(t *testing.T, from, to int, later []timedTarget) *simDesk {
	t.Helper()
	saved := RetargetGap
	RetargetGap = 200 * time.Millisecond
	defer func() { RetargetGap = saved }()

	d := &Desk{chMove: make(chan int, 1)}
	readings := newSlot[reading]()
	sim := newSimDesk(from, readings)
	defer close(sim.stop)

	done := make(chan struct{})
	d.moving.Store(true)
	go func() {
		defer close(done)
		d.runMove(sim, readings, nil, to)
	}()

	start := time.Now()
	for _, l := range later {
		time.Sleep(time.Until(start.Add(l.at)))
		d.newTargetHeigh(l.target)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the move never finished")
	}
	return sim
}

type timedTarget struct {
	at     time.Duration
	target int
}

// The case this was built for: 0 to 1000, then 1500 and 2000 on the way.
// Both are further on the same way, so the desk runs to 1000, stops there
// once, and carries on to the newest - no halt, and no stop anywhere but
// the end of the first leg.
func TestMoveExtendsInsteadOfStopping(t *testing.T) {
	sim := simMove(t, 0, 1000, []timedTarget{{50 * time.Millisecond, 1500}, {120 * time.Millisecond, 2000}})
	pos, halts, rests := sim.state()
	if pos != 2000 {
		t.Errorf("ended at %d, want 2000", pos)
	}
	if halts != 0 {
		t.Errorf("%d halts: a different height reached the moving desk", halts)
	}
	if len(rests) != 2 || rests[0] != 1000 {
		t.Errorf("came to rest at %v, want once at 1000 and then at 2000", rests)
	}
}

// A further target that arrives after the first leg is over starts a new
// move instead; nothing is lost by it arriving late.
func TestMoveExtendsWhenTheNextTargetArrivesLate(t *testing.T) {
	sim := simMove(t, 0, 400, []timedTarget{{80 * time.Millisecond, 1200}})
	pos, halts, _ := sim.state()
	if pos != 1200 || halts != 0 {
		t.Errorf("ended at %d with %d halts, want 1200 and none", pos, halts)
	}
}

// A nearer target cannot wait for the end of the leg - the desk would run
// past it - so it still interrupts, by coasting rather than halting.
func TestMoveNearerTargetStillInterrupts(t *testing.T) {
	sim := simMove(t, 0, 2000, []timedTarget{{100 * time.Millisecond, 1200}})
	pos, halts, rests := sim.state()
	if halts != 0 {
		t.Errorf("%d halts", halts)
	}
	if pos != 1200 {
		t.Errorf("ended at %d, want 1200 (rests %v)", pos, rests)
	}
	if len(rests) < 1 || rests[0] >= 2000 {
		t.Errorf("rests %v: the desk should have been stopped short of 2000", rests)
	}
}

// Asking for the current target again cancels a queued one.
func TestMoveRepeatCancelsTheQueuedTarget(t *testing.T) {
	sim := simMove(t, 0, 1000, []timedTarget{{50 * time.Millisecond, 2000}, {100 * time.Millisecond, 1000}})
	pos, halts, rests := sim.state()
	if pos != 1000 || halts != 0 || len(rests) != 1 {
		t.Errorf("ended at %d, %d halts, rests %v; want to stop once at 1000", pos, halts, rests)
	}
}
