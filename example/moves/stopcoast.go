package main

// The stopcoast scenario compares ways a reversal can bring the desk to
// rest before it heads back.
//
// Coasting is slow. When the writes stop, a DPG1M keeps full speed for
// about 0.85s and then takes about 0.4s to come to rest, so a reversal at
// full speed carries on for some 40mm in the wrong direction before it can
// turn. The methods, chosen with -reversal-methods:
//
//   - coast: Move alone - it goes quiet and lets the desk run down.
//   - stop:  Move, then Stop at once. Measured: no different from coast.
//     Stop does not brake a desk heading for a height.
//   - halt:  Move with desk.HaltOnRetarget, which writes the new height the
//     moment the reversal is asked for. A different height reaching a
//     travelling desk halts it rather than redirecting it; the question is
//     whether that halt is quicker than the coast.
//
// Each trial climbs from -low towards -high, and once the desk is well
// under way asks it to go back to -low. Three things are measured from the
// moment of the reversal:
//
//   - rest: how long until the desk reports standing still
//   - overrun: how far it carried on the wrong way
//   - reverse: how long until it reports moving the other way
//
// "reverse" includes Move's own wait before writing the height that
// starts the new move: RetargetGap, timed from the last height written.
// The threshold scenario puts the controller's own limit at about 800ms
// of silence - 800ms is a coin-flip, 900ms always worked - which is why
// RetargetGap defaults to 1s. This scenario pins it there, so that a
// different default cannot quietly change the comparison. For a halt the last write is the halting one, so its
// reverse time is at least that second whatever the halt itself does.
//
// The methods alternate, so a desk that behaves differently as the run
// goes on affects each equally.

import (
	"fmt"
	"strings"
	"time"

	deskpkg "github.com/gomi-source/linak-dpg/desk"
)

const (
	// reverseAfter is how far into the climb the reversal is asked for,
	// as a fraction of the distance: far enough in that the desk is at
	// full speed, far enough from -high that it does not arrive first.
	reverseAfter = 0.4

	// reverseWindow bounds the wait for the desk to turn.
	reverseWindow = 8 * time.Second

	// stopcoastGap is the RetargetGap this scenario runs with; see above.
	stopcoastGap = time.Second
)

var reversalMethods = []string{"coast", "stop", "halt"}

func parseReversalMethods(spec string) ([]string, error) {
	var out []string
	for _, m := range strings.Split(spec, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if !contains(reversalMethods, m) {
			return nil, fmt.Errorf("-reversal-methods: %q is not one of %s", m, strings.Join(reversalMethods, ", "))
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-reversal-methods: none given")
	}
	return out, nil
}

type stopTrial struct {
	method    string
	rest      time.Duration
	overrun   int
	reverse   time.Duration
	turned    bool
	restSeen  bool
	skipped   string
	atReverse int
}

func runStopCoast(r *runner) error {
	saved := deskpkg.RetargetGap
	deskpkg.RetargetGap = stopcoastGap
	defer func() { deskpkg.RetargetGap = saved }()
	r.rec.note(fmt.Sprintf("RetargetGap is %v for this scenario (was %v)", stopcoastGap, saved))

	n := len(r.o.reversalMethods) * r.o.stopRepeat
	var trials []stopTrial
	for i := 0; i < n; i++ {
		method := r.o.reversalMethods[i%len(r.o.reversalMethods)]
		r.rec.printf("\n  trial %d/%d: %s\n", i+1, n, method)
		trials = append(trials, r.reversal(method))
	}
	reportStopCoast(r, trials, r.o.reversalMethods)
	return nil
}

// reversal runs one trial.
func (r *runner) reversal(method string) stopTrial {
	t := stopTrial{method: method}

	// Start every trial from rest at -low, with the controller long past
	// any wait it might want.
	r.quietFor(thresholdQuiet)
	if at, ok := r.lastKnown(); !ok || !sameDest(at, r.o.low) {
		if err := r.moveTo(r.o.low); err != nil {
			t.skipped = fmt.Sprintf("positioning move failed: %v", err)
			return t
		}
		r.settleAt(r.o.low)
		r.quietFor(thresholdQuiet)
	}

	// Set before the climb's Move starts its goroutine, and cleared once
	// the move is over, so the move reads it without a race.
	deskpkg.HaltOnRetarget = method == "halt"
	defer func() { deskpkg.HaltOnRetarget = false }()

	r.newestReading()
	if err := r.moveTo(r.o.high); err != nil {
		t.skipped = fmt.Sprintf("climb failed to start: %v", err)
		return t
	}

	// Wait until the desk is well into the climb.
	mark := r.o.low + int(float64(r.o.high-r.o.low)*reverseAfter)
	deadline := time.After(reverseWindow)
	for climbing := true; climbing; {
		select {
		case rd := <-r.readings:
			if rd.speed > 0 && rd.extension >= mark {
				t.atReverse, climbing = rd.extension, false
			}
		case <-deadline:
			t.skipped = "the desk never got far enough into the climb"
			r.rec.note(t.skipped)
			r.settleAt(r.o.high)
			return t
		}
	}

	// The reversal. Move goes quiet on a retarget, which is what starts
	// the coast - unless HaltOnRetarget has it write the new height at
	// once. The Stop, when there is one, follows at once.
	reversedAt := time.Now()
	if err := r.moveTo(r.o.low); err != nil {
		t.skipped = fmt.Sprintf("reversal failed: %v", err)
		return t
	}
	if method == "stop" {
		r.act("Stop()")
		if err := r.d.Stop(); err != nil {
			r.rec.note(fmt.Sprintf("Stop failed: %v", err))
		}
	}

	peak := t.atReverse
	deadline = time.After(reverseWindow)
	for !t.turned {
		select {
		case rd := <-r.readings:
			if rd.extension > peak {
				peak = rd.extension
			}
			if rd.speed == 0 && !t.restSeen {
				t.rest, t.restSeen = rd.at.Sub(reversedAt), true
			}
			if rd.speed < 0 {
				t.reverse, t.turned = rd.at.Sub(reversedAt), true
			}
		case <-deadline:
			r.rec.note(fmt.Sprintf("the desk did not turn within %v", reverseWindow))
			t.overrun = peak - t.atReverse
			r.settleAt(r.o.low)
			return t
		}
	}
	t.overrun = peak - t.atReverse
	r.rec.note(fmt.Sprintf("at rest after %v, %d tenths past the reversal point; heading back after %v",
		t.rest.Round(time.Millisecond), t.overrun, t.reverse.Round(time.Millisecond)))

	r.settleAt(r.o.low)
	return t
}

func sameDest(a, b int) bool { return abs(a-b) <= arrivalTolerance }

func reportStopCoast(r *runner, trials []stopTrial, methods []string) {
	p := r.rec.printf
	p("\n  %-7s %-8s %-9s %-12s %s\n", "method", "rest", "overrun", "reverse", "reversal point")
	type sum struct {
		rest, reverse time.Duration
		overrun, n    int
		notTurned     int
	}
	sums := map[string]*sum{}
	for _, m := range methods {
		sums[m] = &sum{}
	}
	for _, t := range trials {
		if t.skipped != "" {
			p("  %-7s skipped: %s\n", t.method, t.skipped)
			continue
		}
		rest, reverse := "-", "did not turn"
		if t.restSeen {
			rest = t.rest.Round(time.Millisecond).String()
		}
		if t.turned {
			reverse = t.reverse.Round(time.Millisecond).String()
		}
		p("  %-7s %-8s %-9s %-12s %d\n", t.method, rest, fmt.Sprintf("%d", t.overrun), reverse, t.atReverse)

		s := sums[t.method]
		if !t.turned {
			s.notTurned++
			continue
		}
		s.n++
		s.rest += t.rest
		s.reverse += t.reverse
		s.overrun += t.overrun
	}

	p("\n  averages over trials that turned (overrun in tenths of a millimetre):\n")
	for _, m := range methods {
		s := sums[m]
		if s.n == 0 {
			p("    %-6s no trial turned\n", m)
			continue
		}
		p("    %-6s rest %v, overrun %d, reverse %v", m,
			(s.rest / time.Duration(s.n)).Round(time.Millisecond),
			s.overrun/s.n,
			(s.reverse / time.Duration(s.n)).Round(time.Millisecond))
		if s.notTurned > 0 {
			p(" - and %d did not turn", s.notTurned)
		}
		p("\n")
	}
}
