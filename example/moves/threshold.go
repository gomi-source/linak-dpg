package main

// The threshold scenario measures one number: how long the controller has
// to hear nothing after a move before it will act on a new height.
//
// It exists because the evidence for that number so far is circumstantial.
// Across one run of the default scenarios, every new height sent after a
// second or more without writes moved the desk, and the one sent after
// 0.79s was ignored - along with every retry after it, 200ms apart. That
// fits a controller that ignores a height arriving too soon after the last
// one and restarts its clock on each write, but it also fits other rules,
// and a fix to Move should rest on a measurement rather than a fit.
//
// So each trial does exactly one thing differently. It brings the desk to
// rest with an ordinary Move, so there is an arrival to measure from; stays
// silent for one of -gaps, timed from the last height that Move wrote; then
// writes a single height straight to ReferenceInput - not through Move,
// which would add its own gap and retries - and watches whether the desk
// starts. Nothing sustains a probe that works, so the desk moves a few
// centimetres on its own momentum and stops, which keeps each trial short
// and inside -low and -high.
//
// Two clocks are recorded for every trial, because they are the two
// candidate rules: time since the last write, and time since the desk
// reported standing still. They differ by how long the desk took to stop
// after its last command, and the table shows which one the outcome
// follows.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gomi-source/linak-dpg"
)

const (
	// thresholdQuiet is the silence before each trial's positioning move -
	// far above anything the controller has been seen to need, so that the
	// arrival itself is never in question.
	thresholdQuiet = 2500 * time.Millisecond

	// probeWindow is how long to watch for the desk to start after a probe.
	// A desk that is going to move reports within about 200ms.
	probeWindow = 1500 * time.Millisecond

	// probeOffset keeps the two starting points apart, in tenths of a
	// millimetre, so every trial has a real move to arrive from.
	probeOffset = 150
)

type trial struct {
	gap       time.Duration // the silence asked for
	silence   time.Duration // time since the last write, when the probe went out
	sinceRest time.Duration // time since the desk reported standing still
	arrivedUp bool          // direction of the positioning move
	probeUp   bool          // direction the probe asked for
	moved     bool
	after     time.Duration // probe to first report of movement
	skipped   string        // why the trial measured nothing, when it did not
}

func parseGaps(spec string) ([]time.Duration, error) {
	var out []time.Duration
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		ms, err := strconv.Atoi(f)
		if err != nil || ms < 0 {
			return nil, fmt.Errorf("-gaps: %q is not a whole number of milliseconds", f)
		}
		out = append(out, time.Duration(ms)*time.Millisecond)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-gaps: no silences given")
	}
	return out, nil
}

func runThreshold(r *runner) error {
	c, err := (&dpg.GATT{}).GetCharacteristic(r.d.Device(), dpg.ServiceUUIDReferenceInput, dpg.CharacteristicUUIDReferenceInput)
	if err != nil {
		return fmt.Errorf("reference input characteristic: %w", err)
	}

	mid := r.o.low + (r.o.high-r.o.low)/2
	starts := [2]int{mid - probeOffset, mid + probeOffset}

	// Every other repetition runs the gaps backwards, so a desk that drifts
	// over the run - warming up, say - shows up as a disagreement between
	// the two passes rather than as a slope in the result.
	var order []time.Duration
	for rep := 0; rep < r.o.gapRepeat; rep++ {
		for i := range r.o.gaps {
			if rep%2 == 0 {
				order = append(order, r.o.gaps[i])
			} else {
				order = append(order, r.o.gaps[len(r.o.gaps)-1-i])
			}
		}
	}

	var trials []trial
	for i, gap := range order {
		start := starts[i%2]
		// The probe's direction changes every two trials, independently of
		// where the trial starts, so it cannot be confused with the
		// direction of the arrival.
		probeTarget := r.o.high
		if (i/2)%2 == 1 {
			probeTarget = r.o.low
		}

		r.rec.printf("\n  trial %d/%d: arrive at %d, stay silent %v, then write %d once\n",
			i+1, len(order), start, gap, probeTarget)
		t := r.probe(c, gap, start, probeTarget)
		trials = append(trials, t)
	}

	reportThreshold(r, trials)
	return nil
}

// probe runs one trial.
func (r *runner) probe(c dpg.Characteristic, gap time.Duration, start, probeTarget int) trial {
	t := trial{gap: gap}

	r.quietFor(thresholdQuiet)

	issued := time.Now()
	if err := r.moveTo(start); err != nil {
		t.skipped = fmt.Sprintf("positioning move failed: %v", err)
		return t
	}
	r.settleAt(start)

	lastWrite := r.rec.lastWrite()
	_, rest, dir := r.motion()
	if lastWrite.Before(issued) || rest.Before(issued) {
		t.skipped = "no positioning move happened, so there was no arrival to measure from"
		r.rec.note(t.skipped)
		return t
	}
	t.arrivedUp = dir > 0

	if wait := time.Until(lastWrite.Add(gap)); wait > 0 {
		time.Sleep(wait)
	}
	r.newestReading() // what came before the probe is not an answer to it

	at, _ := r.lastKnown()
	t.probeUp = probeTarget > at
	probeAt := time.Now()
	t.silence = probeAt.Sub(r.rec.lastWrite())
	t.sinceRest = probeAt.Sub(rest)

	r.rec.act(fmt.Sprintf("write %d once - %v after the last write, %v after coming to rest",
		probeTarget, t.silence.Round(time.Millisecond), t.sinceRest.Round(time.Millisecond)))
	b := dpg.NewHeight(probeTarget).Bytes()
	if _, err := c.WriteWithoutResponse(b[:]); err != nil {
		t.skipped = fmt.Sprintf("probe write failed: %v", err)
		r.rec.note(t.skipped)
		return t
	}

	deadline := time.After(probeWindow)
	for !t.moved {
		select {
		case rd := <-r.readings:
			if rd.speed != 0 {
				t.moved, t.after = true, rd.at.Sub(probeAt)
			}
		case <-deadline:
			r.rec.note(fmt.Sprintf("ignored: no movement within %v", probeWindow))
			return t
		}
	}
	r.rec.note(fmt.Sprintf("moved: first report of movement %v after the probe", t.after.Round(time.Millisecond)))

	// Nothing sustains the probe, so the desk runs on for a moment and
	// stops by itself. Wait for that before the next trial starts.
	r.waitRest(5 * time.Second)
	return t
}

// quietFor waits until the desk is still and no height has been written
// for at least d.
func (r *runner) quietFor(d time.Duration) {
	for {
		seen, _, _ := r.motion()
		if time.Since(r.rec.lastWrite()) >= d && time.Since(seen) >= 500*time.Millisecond {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitRest waits for a speed 0 report, or for the reports to stop.
func (r *runner) waitRest(timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		select {
		case rd := <-r.readings:
			if rd.speed == 0 {
				return
			}
		case <-time.After(time.Second):
			return
		case <-deadline:
			return
		}
	}
}

func reportThreshold(r *runner, trials []trial) {
	p := r.rec.printf
	p("\n  %-8s %-9s %-11s %-8s %-6s %s\n", "gap", "silence", "since rest", "arrived", "probe", "result")
	for _, t := range trials {
		if t.skipped != "" {
			p("  %-8v skipped: %s\n", t.gap, t.skipped)
			continue
		}
		result := "ignored"
		if t.moved {
			result = fmt.Sprintf("moved (%v)", t.after.Round(time.Millisecond))
		}
		p("  %-8v %-9v %-11v %-8s %-6s %s\n",
			t.gap, t.silence.Round(time.Millisecond), t.sinceRest.Round(time.Millisecond),
			updown(t.arrivedUp), updown(t.probeUp), result)
	}

	type tally struct{ moved, tried int }
	byGap := map[time.Duration]*tally{}
	for _, t := range trials {
		if t.skipped != "" {
			continue
		}
		if byGap[t.gap] == nil {
			byGap[t.gap] = &tally{}
		}
		byGap[t.gap].tried++
		if t.moved {
			byGap[t.gap].moved++
		}
	}
	gaps := make([]time.Duration, 0, len(byGap))
	for g := range byGap {
		gaps = append(gaps, g)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })

	p("\n  moved, by silence:")
	for _, g := range gaps {
		p("  %v %d/%d", g, byGap[g].moved, byGap[g].tried)
	}
	p("\n")
}

func updown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}
