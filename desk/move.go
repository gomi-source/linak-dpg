package desk

import (
	"fmt"
	"time"

	"github.com/gomi-source/linak-dpg"
)

// Moving a DPG desk is not one write. The controller moves only while it
// is being told where to go, so the target is rewritten continuously until
// the desk reports it has arrived, and Move returns as soon as the first
// write is out with a goroutine doing the rest.
//
// Three things make that harder than it sounds.
//
// The desk only accepts a different height once it has come to rest.
// Rewriting the same height is what sustains movement; stop writing and it
// stops, which is how a retarget brings it to rest - it goes quiet, waits
// for the stream to report speed 0, and only then writes the new height.
// Writing one sooner halts the desk instead of redirecting it. That pause
// belongs to us and is not read as the desk stopping of its own accord. A
// reversal additionally sends Stop, to bring it to rest deliberately
// rather than by coasting.
//
// A halt is not an arrival, and it is not a thing to push through. The
// desk reports speed 0 both when it has reached the target and when it has
// stopped short - and one reason it stops short is that it has hit
// something. The controller's own safety stop is the only protection a
// person's hand has here, so a halt that is not an arrival ends the move:
// the target is never re-commanded to make a stopped desk try again. The
// old code's "keep writing until something moves" is exactly what must not
// happen once the desk has decided to stop.
//
// Position reports must never be allowed to queue. They arrive on their
// own goroutine each, so a blocking handoff would leave a backlog of
// readings from seconds ago, and a speed 0 recorded before a retarget
// would then surface after it and be read as that move finishing. Readings
// go into a one-slot holder instead: the loop always sees the newest, and
// a reading nobody collected is discarded rather than queued.

const (
	// arrivalTolerance is how close counts as arrived, in tenths of a
	// millimetre. The desk stops under its own control and does not land
	// exactly on the requested value.
	arrivalTolerance = 20

	// stallTimeout ends a move that never reports anything at all: the
	// desk is ignoring the writes, which usually means the owner bit is
	// not set.
	stallTimeout = 5 * time.Second

	// startGrace is how long the target keeps being written to a desk that
	// has not begun moving. It is deliberately short: a desk that will not
	// start is either deaf to us or blocked, and repeating the command at
	// something that is not moving is the case worth being careful about.
	// Once the desk has moved, a later halt is never re-commanded at all.
	startGrace = 1500 * time.Millisecond

	// progressThreshold is how much the height has to change to count as
	// progress and restart the stall timer, in tenths of a millimetre. A
	// stationary desk still reports, and its readings jitter slightly.
	progressThreshold = 5

	// stopSettle bounds the wait for the desk to come to rest after a
	// reversal. Reaching it is not fatal - the retarget proceeds anyway -
	// so it only stops a reversal hanging forever if the desk goes quiet.
	stopSettle = 3 * time.Second
)

// RetargetGap is the minimum quiet period between the last write of one
// height and the first write of the next. A retarget waits for the desk to
// report speed 0 *and* for this to elapse, whichever is later.
//
// The speed reading is the real condition - the desk accepts a new height
// only once it has stopped - and this is the floor under it: 800ms was
// arrived at by trial against a DPG1M, before the stream was being watched
// for rest, and keeping it means the new behaviour is never quicker off
// the mark than what was known to work. Waiting for rest as well is what
// covers a desk still decelerating after 800ms, which a fixed pause could
// not.
//
// It is a package variable so a different controller can be given a
// different figure; nothing reads it after a move has begun, so change it
// at startup.
var RetargetGap = 800 * time.Millisecond

// reading is one ReferenceOutput report: where the desk is and how fast it
// is going.
type reading struct {
	extension int
	speed     int
}

// Move sends the desk to an extension, in tenths of a millimetre above its
// own base - not above the floor; see BaseOffset for the other frame.
//
// It returns as soon as the first target has been written; a background
// goroutine keeps the target in front of the desk while it travels.
//
// Calling Move again while a move is running retargets that move rather
// than starting a second one. A reversal stops the desk first and waits
// for it to come to rest; a new target in the direction already being
// travelled is simply written.
//
// A desk that stops short of the target is left stopped. It stops by
// itself when it meets resistance, and telling it to try again would
// defeat the only protection whatever it met has, so the move ends and
// says so. Deciding whether to ask again belongs to the caller, who may
// know the way is clear; this package will not do it silently.
//
// Every write is ignored unless TakeOwnership has succeeded, which is what
// a move that never starts usually means.
func (d *Desk) Move(mmx10 int) error {
	if d.moving.Load() {
		d.newTargetHeigh(mmx10)
		return nil
	}
	d.moving.Store(true)

	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDReferenceInput, dpg.CharacteristicUUIDReferenceInput)
	if err != nil {
		d.moving.Store(false)
		return fmt.Errorf("could not move desk: %v", err)
	}

	// One slot, not a queue: the loop wants the desk's current state, and
	// a reading it never collected is of no use to anyone.
	readings := newSlot[reading]()
	removeCallback := d.roSub.AddReferenceOutputCallback(func(extension, speed int) {
		readings.put(reading{extension: extension, speed: speed})
	})

	if _, err := c.WriteWithoutResponse(target(mmx10)); err != nil {
		removeCallback()
		d.moving.Store(false)
		return fmt.Errorf("could not move desk: %v", err)
	}

	go func() {
		defer d.moving.Store(false)
		defer removeCallback()
		d.runMove(c, readings, mmx10)
	}()

	return nil
}

// runMove sustains the move until the desk arrives, stops, or never
// starts. It writes the target repeatedly *while the desk is moving* -
// which is what keeps it moving - and stops writing the moment it is not.
func (d *Desk) runMove(c dpg.Characteristic, readings *slot[reading], mmx10 int) {
	log := d.device.Logger()

	var last reading
	haveLast := false
	movedYet := false
	written := time.Now()

	stall := time.NewTimer(stallTimeout)
	defer stall.Stop()

	write := func() bool {
		if _, err := c.WriteWithoutResponse(target(mmx10)); err != nil {
			log.Warn("move write failed", "target_tenths_mm", mmx10, "err", err)
			return false
		}
		written = time.Now()
		return true
	}

	for {
		select {
		case r := <-readings.ch:
			if haveLast && abs(r.extension-last.extension) >= progressThreshold {
				resetTimer(stall, stallTimeout)
			}
			last, haveLast = r, true

			if r.speed != 0 {
				// Moving freely: keep the target in front of it.
				movedYet = true
				if !write() {
					return
				}
				continue
			}

			// Stopped. Why it stopped decides everything.
			switch {
			case abs(r.extension-mmx10) <= arrivalTolerance:
				log.Debug("move finished", "target_tenths_mm", mmx10, "at_tenths_mm", r.extension)

			case movedYet:
				// It was moving and it stopped short. That may be an
				// obstruction, and the desk's safety stop is what protects
				// whatever it hit - so the move ends here. Re-commanding
				// would drive it back into the obstruction.
				log.Warn("move stopped short of its target; not re-commanding",
					"target_tenths_mm", mmx10,
					"at_tenths_mm", r.extension,
					"short_by_tenths_mm", abs(mmx10-r.extension),
					"hint", "the desk stops by itself when it meets resistance")

			case time.Since(written) < startGrace:
				// Not started yet. A desk takes a moment to pick up the
				// first target, so keep offering it - briefly.
				if !write() {
					return
				}
				continue

			default:
				log.Warn("desk did not start moving",
					"target_tenths_mm", mmx10,
					"at_tenths_mm", r.extension,
					"hint", "the desk ignores writes unless TakeOwnership has succeeded, or it is blocked")
			}
			return

		case newTarget := <-d.chMove:
			// A retarget is a new move, not an assignment. Writing a
			// different height too soon after the last one halts the desk,
			// so stop commanding the old target and wait before starting
			// the new one. Anything the desk reports in that window - speed
			// 0 included - is the old move ending, not a fault.
			log.Debug("move retargeting", "from_tenths_mm", mmx10, "to_tenths_mm", newTarget)
			last, haveLast = waitReadyForNewTarget(readings, last, haveLast, written.Add(RetargetGap))

			// A burst of targets costs one gap, not one each: whatever
			// arrived while we were quiet supersedes what started it. Only
			// the last one is ever written, so nudging a desk upwards four
			// times in a second interrupts it once.
			if latest, ok := latestTarget(d.chMove); ok {
				log.Debug("move coalescing", "superseded_tenths_mm", newTarget, "to_tenths_mm", latest)
				newTarget = latest
			}

			// Decided after the gap, against the final target: the desk has
			// had no commands for RetargetGap by now, so this asks whether
			// it is still travelling the wrong way rather than whether it
			// was when the first of the burst arrived.
			if haveLast && reverses(last, newTarget) {
				log.Debug("move reversing", "to_tenths_mm", newTarget, "at_tenths_mm", last.extension)
				if err := d.Stop(); err != nil {
					log.Warn("could not stop before reversing", "err", err)
				}
				last, haveLast = waitUntilStopped(readings, last), true
			}

			mmx10 = newTarget
			movedYet = false // the desk has to start again from rest
			if !write() {
				return
			}
			resetTimer(stall, stallTimeout)

		case <-stall.C:
			log.Warn("move timed out with no position reports",
				"target_tenths_mm", mmx10,
				"hint", "the desk ignores writes unless TakeOwnership has succeeded")
			return
		}
	}
}

// reverses reports whether reaching newTarget means travelling the other
// way from where the desk is heading now. Only a reversal needs the desk
// stopped first; a new target further along, or short of the old one but
// still ahead, is just a change of destination.
func reverses(last reading, newTarget int) bool {
	if last.speed == 0 {
		return false // already at rest; nothing to reverse
	}
	return sign(newTarget-last.extension) == -sign(last.speed)
}

// waitUntilStopped consumes readings until the desk reports it has come to
// rest, or stopSettle passes. Returning the last reading keeps the caller's
// idea of where the desk is current.
func waitUntilStopped(readings *slot[reading], last reading) reading {
	deadline := time.After(stopSettle)
	for {
		select {
		case r := <-readings.ch:
			if r.speed == 0 {
				return r
			}
			last = r
		case <-deadline:
			return last
		}
	}
}

// waitReadyForNewTarget writes nothing and waits for the desk to be ready
// for a different height: at rest, and at least gapUntil. Not writing is
// what brings it to rest, so this is the stopping as much as the waiting.
//
// Readings here are not classified. The desk coming to rest during it is
// the old move ending - the thing being waited for - rather than a stop
// worth reporting, and they are consumed only to keep the caller's idea of
// where the desk is current.
//
// It gives up after stopSettle so a desk that goes quiet without reporting
// rest cannot hang the move; the retarget then proceeds anyway, which is
// no worse than the fixed pause it replaced.
func waitReadyForNewTarget(readings *slot[reading], last reading, haveLast bool, gapUntil time.Time) (reading, bool) {
	atRest := haveLast && last.speed == 0
	gapPassed := false

	gap := time.NewTimer(time.Until(gapUntil))
	defer gap.Stop()
	deadline := time.NewTimer(stopSettle)
	defer deadline.Stop()

	for {
		if atRest && gapPassed {
			return last, haveLast
		}
		select {
		case r := <-readings.ch:
			last, haveLast = r, true
			atRest = r.speed == 0
		case <-gap.C:
			gapPassed = true
		case <-deadline.C:
			return last, haveLast
		}
	}
}

// latestTarget takes the newest target waiting, if any, discarding older
// ones. newTargetHeigh already replaces rather than queues, so this is at
// most one value - the loop drains it after the gap so a burst collapses
// into a single write.
func latestTarget(ch chan int) (int, bool) {
	var latest int
	found := false
	for {
		select {
		case v := <-ch:
			latest, found = v, true
		default:
			return latest, found
		}
	}
}

// target packs an extension for the ReferenceInput characteristic.
func target(mmx10 int) []byte {
	b := dpg.NewHeight(mmx10).Bytes()
	return b[:]
}

func (d *Desk) newTargetHeigh(mmx10 int) {
	// Non-blocking: replace previous request
	select {
	case d.chMove <- mmx10:
	default:
		// queue full -> replace old request
		select {
		case <-d.chMove:
		default:
		}
		select {
		case d.chMove <- mmx10:
		default:
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}
