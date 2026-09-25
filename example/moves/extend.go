package main

// The extend scenario asks one question: must a travelling desk be brought
// to rest before it will take a different height in the same direction?
//
// Move assumes so. On a retarget it goes quiet, lets the desk coast to
// rest, waits out RetargetGap and only then writes the new height - which
// costs two seconds or more every time. The assumption rests on reversals:
// a height back the way the desk came, written mid-move, halts it and
// makes the error characteristic send 01 00 10. Nobody has written a
// height further along the same direction to a moving desk and watched.
// If the controller simply follows it, Move could extend a move without
// stopping, and a user nudging a target up and up would see one smooth
// move rather than a stop at every nudge.
//
// Measured on a DPG1M: it does not. All eight trials - further and
// nearer, up and down - halted 8-10mm after the switch and sent 01 00 10,
// so Move now queues a further target until the end of the current leg
// instead of writing it; see desk/move.go.
//
// So this scenario bypasses Move entirely and does what Move would do if
// the stop were unnecessary. It starts the desk towards a first height by
// writing to ReferenceInput itself, sustains that height on every report
// the way Move does, and once the desk is at full speed switches to a
// second height in the same direction, sustaining that one from then on.
// Two kinds of trial, each run going up and going down:
//
//   - further: the second height lies beyond the first - the case that
//     matters for nudging a target along.
//   - nearer:  the second height lies between the desk and the first -
//     Move's own retarget scenario, with the stop left out.
//
// What the desk does next is the answer:
//
//   - followed: it carried on without coming to rest and stopped at the
//     second height. The minimum speed after the switch shows whether it
//     so much as slowed.
//   - halted: it came to rest short of the second height, as a reversal
//     does. The distance after the switch is comparable with the 10mm a
//     halted reversal overran.
//   - stopped at the first height: it ignored the switch.
//   - ran past: it travelled beyond the nearer height, towards the first.
//
// Error frames are recorded per trial. Writing stops the moment the desk
// comes to rest, runs past, or reports a collision, exactly as Move's own
// rules would have it - a halt is never pushed through.

import (
	"fmt"
	"strings"
	"time"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription/controlerror"
	"github.com/gomi-source/linak-dpg/subscription/referenceoutput"
)

const (
	// extendMinSpan is the least distance between -low and -high this
	// scenario will work with: room to reach full speed, switch, and still
	// tell a halt from an arrival.
	extendMinSpan = 1000

	// extendFirst is where the first height lies for a further trial, as a
	// fraction of the span, and extendNearer where the second lies for a
	// nearer one.
	extendFirst  = 0.45
	extendNearer = 0.55

	// switchSpeed is how fast the desk must be going before the switch:
	// well under way, but a DPG1M's full speed is 6224, so not at the very
	// top of the ramp.
	switchSpeed = 5000

	// switchRoom is the least distance, in tenths of a millimetre, left to
	// the height the desk is braking for when the switch happens. A DPG1M
	// starts braking for its target about 6mm out and a halted reversal
	// ran on about 10mm, so 30mm keeps a halt, a coast and an arrival
	// apart.
	switchRoom = 300

	// extendWindow bounds one trial from the switch.
	extendWindow = 15 * time.Second

	// startAttempts is how often the first height is offered to a desk
	// that has not started, a RetargetGap apart, as Move does.
	startAttempts = 3
)

type extendTrial struct {
	kind     string // further or nearer
	up       bool
	start    int
	first    int
	second   int
	switchAt int

	result   string
	endedAt  int
	after    time.Duration // switch to rest
	minSpeed int           // lowest |speed| after the switch, away from the second height
	errors   []string
	skipped  string
}

func runExtend(r *runner) error {
	span := r.o.high - r.o.low
	if span < extendMinSpan {
		r.rec.note(fmt.Sprintf("-low %d and -high %d are %d apart; this scenario needs at least %d, so it does not run",
			r.o.low, r.o.high, span, extendMinSpan))
		return nil
	}

	c, err := (&dpg.GATT{}).GetCharacteristic(r.d.Device(), dpg.ServiceUUIDReferenceInput, dpg.CharacteristicUUIDReferenceInput)
	if err != nil {
		return fmt.Errorf("reference input characteristic: %w", err)
	}

	at := func(f float64, up bool) int {
		if up {
			return r.o.low + int(float64(span)*f)
		}
		return r.o.high - int(float64(span)*f)
	}
	var plan []extendTrial
	for rep := 0; rep < r.o.extendRepeat; rep++ {
		for _, kind := range []string{"further", "nearer"} {
			for _, up := range []bool{true, false} {
				t := extendTrial{kind: kind, up: up, start: r.o.low}
				if !up {
					t.start = r.o.high
				}
				end := r.o.high
				if !up {
					end = r.o.low
				}
				if kind == "further" {
					t.first, t.second = at(extendFirst, up), end
				} else {
					t.first, t.second = end, at(extendNearer, up)
				}
				plan = append(plan, t)
			}
		}
	}

	var trials []extendTrial
	for i, t := range plan {
		r.rec.printf("\n  trial %d/%d: %s %s - from %d head for %d, then switch to %d at full speed\n",
			i+1, len(plan), t.kind, updown(t.up), t.start, t.first, t.second)
		trials = append(trials, r.extend(c, t))
	}
	// Leave the controller alone before whatever runs next: the last
	// write here was not Move's, so Move does not know to wait for it.
	r.quietFor(thresholdQuiet)

	reportExtend(r, trials)
	return nil
}

// extend runs one trial.
func (r *runner) extend(c dpg.Characteristic, t extendTrial) extendTrial {
	// From rest at the start, with the controller long past any wait it
	// might want.
	r.quietFor(thresholdQuiet)
	if at, ok := r.lastKnown(); !ok || !sameDest(at, t.start) {
		if err := r.moveTo(t.start); err != nil {
			t.skipped = fmt.Sprintf("positioning move failed: %v", err)
			return t
		}
		r.settleAt(t.start)
		r.quietFor(thresholdQuiet)
	}
	if r.d.Moving() {
		t.skipped = "Move is still running, and two writers would make the answer meaningless"
		return t
	}
	r.newestReading()
	r.drainErrors()

	dir := 1
	if !t.up {
		dir = -1
	}
	write := func(h int) bool {
		b := dpg.NewHeight(h).Bytes()
		if _, err := c.WriteWithoutResponse(b[:]); err != nil {
			r.rec.note(fmt.Sprintf("write failed: %v", err))
			return false
		}
		return true
	}

	// Start the desk towards the first height, as Move would: offer it,
	// and again a RetargetGap later if nothing happens.
	r.act(fmt.Sprintf("write %d directly, sustained on every report", t.first))
	var last reading
	started := false
	for attempt := 0; attempt < startAttempts && !started; attempt++ {
		if !write(t.first) {
			t.skipped = "the first write failed"
			return t
		}
		deadline := time.After(time.Second + 100*time.Millisecond)
	wait:
		for {
			select {
			case rd := <-r.readings:
				if rd.speed != 0 {
					last, started = rd, true
					break wait
				}
			case <-deadline:
				break wait
			}
		}
	}
	if !started {
		t.skipped = "the desk never started towards the first height"
		r.rec.note(t.skipped)
		return t
	}

	// Sustain the first height until the desk is at full speed with room
	// to spare, then switch.
	target := t.first
	switched := false
	var switchedAt time.Time
	t.minSpeed = -1
	deadline := time.After(extendWindow + 10*time.Second)

	for {
		select {
		case frame := <-r.errs:
			if !switched {
				continue
			}
			t.errors = append(t.errors, describeFrame(frame))
			if code, ok := controlerror.Code(frame); ok && code == controlerror.CodeCollision {
				t.result, t.endedAt = "collision", last.extension
				r.rec.note("collision reported; writing stops")
				return r.extendRest(t, switchedAt)
			}

		case rd := <-r.readings:
			last = rd
			if rd.flags&referenceoutput.FlagCollisionRecovery != 0 {
				if !switched {
					t.skipped = "the desk hit something before the switch"
					r.rec.note(t.skipped)
					r.waitRest(5 * time.Second)
					return t
				}
				t.result, t.endedAt = "collision", rd.extension
				r.rec.note("collision recovery reported; writing stops")
				return r.extendRest(t, switchedAt)
			}

			if rd.speed == 0 {
				t.endedAt = rd.extension
				t.after = rd.at.Sub(switchedAt)
				switch {
				case !switched:
					t.skipped = fmt.Sprintf("the desk stopped at %d before the switch", rd.extension)
					r.rec.note(t.skipped)
					return t
				case sameDest(rd.extension, t.second):
					t.result = "followed"
				case sameDest(rd.extension, t.first):
					t.result = "stopped at the first height"
				default:
					t.result = "halted"
				}
				r.rec.note(fmt.Sprintf("%s: at rest at %d, %v after the switch, %d from where it switched",
					t.result, rd.extension, t.after.Round(time.Millisecond), abs(rd.extension-t.switchAt)))
				t.errors = append(t.errors, r.errorsFor(700*time.Millisecond)...)
				return t
			}

			if !switched {
				room := dir * (t.first - rd.extension)
				if t.kind == "nearer" {
					room = dir * (t.second - rd.extension)
				}
				if room < switchRoom {
					t.skipped = fmt.Sprintf("the desk was within %d of the height before reaching full speed; widen -low and -high", switchRoom)
					r.rec.note(t.skipped)
					return r.extendRest(t, time.Now())
				}
				if abs(rd.speed) >= switchSpeed {
					switched, switchedAt, t.switchAt = true, time.Now(), rd.extension
					target = t.second
					r.act(fmt.Sprintf("switch: write %d at %d, speed %d", target, rd.extension, rd.speed))
				}
			} else {
				// Away from the second height, so braking for it does
				// not count as slowing for the switch.
				if abs(t.second-rd.extension) > 200 && (t.minSpeed < 0 || abs(rd.speed) < t.minSpeed) {
					t.minSpeed = abs(rd.speed)
				}
				if dir*(rd.extension-t.second) > arrivalTolerance {
					t.result, t.endedAt = "ran past", rd.extension
					r.rec.note(fmt.Sprintf("ran past %d towards %d; writing stops", t.second, t.first))
					return r.extendRest(t, switchedAt)
				}
				if time.Since(switchedAt) > extendWindow {
					t.result, t.endedAt = "still moving", rd.extension
					r.rec.note(fmt.Sprintf("still moving %v after the switch; writing stops", extendWindow))
					return r.extendRest(t, switchedAt)
				}
			}
			if !write(target) {
				t.skipped = "a sustaining write failed"
				return r.extendRest(t, switchedAt)
			}

		case <-deadline:
			t.result, t.endedAt = "no reports", last.extension
			r.rec.note("the reports stopped")
			return t
		}
	}
}

// extendRest waits, writing nothing, for a trial whose writing has stopped
// to come to rest by itself, and records where.
func (r *runner) extendRest(t extendTrial, switchedAt time.Time) extendTrial {
	r.waitRest(5 * time.Second)
	if at, ok := r.lastKnown(); ok {
		t.endedAt = at
	}
	t.after = time.Since(switchedAt)
	t.errors = append(t.errors, r.errorsFor(0)...)
	return t
}

// errorsFor collects error frames that arrive within d, and any already
// waiting.
func (r *runner) errorsFor(d time.Duration) []string {
	var out []string
	deadline := time.After(d)
	for {
		select {
		case f := <-r.errs:
			out = append(out, describeFrame(f))
		case <-deadline:
			for {
				select {
				case f := <-r.errs:
					out = append(out, describeFrame(f))
				default:
					return out
				}
			}
		}
	}
}

func (r *runner) drainErrors() {
	for {
		select {
		case <-r.errs:
		default:
			return
		}
	}
}

func describeFrame(f []byte) string {
	if controlerror.Cleared(f) {
		return "(empty)"
	}
	return hexOf(f)
}

func reportExtend(r *runner, trials []extendTrial) {
	p := r.rec.printf
	p("\n  %-8s %-5s %-6s %-6s %-7s %-28s %-9s %s\n", "kind", "dir", "first", "second", "switch", "result", "min speed", "error frames")
	for _, t := range trials {
		if t.skipped != "" {
			p("  %-8s %-5s skipped: %s\n", t.kind, updown(t.up), t.skipped)
			continue
		}
		result := fmt.Sprintf("%s at %d", t.result, t.endedAt)
		min := "-"
		if t.minSpeed >= 0 {
			min = fmt.Sprintf("%d", t.minSpeed)
		}
		errs := "none"
		if len(t.errors) > 0 {
			errs = strings.Join(t.errors, ", ")
		}
		p("  %-8s %-5s %-6d %-6d %-7d %-28s %-9s %s\n", t.kind, updown(t.up), t.first, t.second, t.switchAt, result, min, errs)
	}
	p("\n  followed with a min speed near full (6224) means the controller took the new height\n")
	p("  without slowing: Move could then extend a move rather than stopping it.\n")
}
