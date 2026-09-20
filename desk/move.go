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
// The desk reports only while it moves. A stationary one says nothing at
// all, so "no reports" is the normal state between moves and the way a
// move that never started announces itself - there is no speed 0 reading
// to classify, because there is no reading.
//
// Position reports must never be allowed to queue. They arrive on their
// own goroutine each, so a blocking handoff would leave a backlog of
// readings from seconds ago, and a speed 0 recorded before a retarget
// would then surface after it and be read as that move finishing. Readings
// go into a one-slot holder instead: the loop always sees the newest, and
// a reading nobody collected is discarded rather than queued.

const (
	// arrivalTolerance is the controller's dead band, in tenths of a
	// millimetre: the largest distance it will not act on. Measured on a
	// DPG1M, which moves for a 1.3mm request and not for 1.2mm.
	//
	// It settles two questions at once, because they are the same physical
	// fact. A target this close is a no-op, so asking for one reports
	// arrival rather than waiting for movement that will never come. And a
	// desk that has stopped this close has arrived, since it cannot get
	// nearer than its own dead band.
	//
	// Too large is the dangerous direction: a value above the dead band
	// makes real moves - ones the desk would perform - report arrival
	// before they start, and nothing then sustains them.
	arrivalTolerance = 12

	// stallTimeout ends a move that never reports anything at all - not a
	// desk that reports standing still, which is handled where the
	// readings are classified, but one that has gone silent. A dropped
	// link and a desk that never had the owner bit look the same here.
	stallTimeout = 5 * time.Second

	// startGrace is how long the target keeps being written to a desk that
	// has not begun moving. It is deliberately short: a desk that will not
	// start is either deaf to us or blocked, and repeating the command at
	// something that is not moving is the case worth being careful about.
	// Once the desk has moved, a later halt is never re-commanded at all.
	startGrace = 1500 * time.Millisecond

	// progressThreshold is how much the height has to change to count as
	// progress and restart the stall timer, in tenths of a millimetre.
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
// Rest is necessary but not sufficient. A DPG1M that had reported speed 0
// and arrived still ignored a different height written 313ms later, so
// coming to a stop does not by itself make the controller ready. What the
// gap is measured from - the last write, or the arrival 57ms after it -
// that observation cannot separate; it is taken from the last write, which
// is the conservative reading.
//
// So the true figure is somewhere above 313ms, and 800ms is known to work.
// Lowering this is the way to find it: the failure is visible and harmless
// - the desk does not move and the move times out saying so.
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
// A move to where the desk already is does nothing, and says so rather
// than waiting: the controller has a dead band and will not start for a
// target close to its current position. That is the usual reason a move
// reports not starting - see arrivalTolerance.
//
// Every write is also ignored unless TakeOwnership has succeeded, which is
// the other reason, and the likelier one only if ownership is not being
// asserted on connect.
func (d *Desk) Move(mmx10 int) error {
	if d.moving.Load() {
		d.newTargetHeigh(mmx10)
		return nil
	}

	d.ensurePositionTracking()

	// A target the desk is already at is a no-op, and it is one we can
	// recognise: asking anyway means five seconds of silence and a warning
	// for a move that was never going to happen.
	if at, ok := d.lastKnownPosition(); ok && sameDestination(mmx10, at) {
		d.device.Logger().Debug("move not needed, desk is already there",
			"target_tenths_mm", mmx10, "at_tenths_mm", at)
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

	// The first write happens in the goroutine, not here: the controller
	// may need to be left alone for the rest of RetargetGap before it will
	// accept this height, and Move must not block for that.
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
		d.noteTargetWritten(mmx10)
		return true
	}

	// A move that follows another too closely is the same mistake as a
	// retarget that does: the controller ignores a different height until
	// it has been left alone. Between two Move calls nothing else enforces
	// that, so it is enforced here, before the first write.
	if wait := d.waitBeforeTarget(mmx10); wait > 0 {
		log.Debug("waiting before a new target", "target_tenths_mm", mmx10, "wait", wait.Round(time.Millisecond))
		time.Sleep(wait)
	}
	if !write() {
		return
	}
	resetTimer(stall, stallTimeout)

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
				// Two things stop a desk starting, and the distance tells
				// them apart: a target within the controller's own dead
				// band is simply ignored, which is the common one now that
				// ownership is asserted on every connect. The distance is
				// logged so the reader does not have to subtract.
				log.Warn("desk did not start moving",
					"target_tenths_mm", mmx10,
					"at_tenths_mm", r.extension,
					"distance_tenths_mm", abs(mmx10-r.extension),
					"hint", "a short distance means the target is inside the desk's dead band and it will not move; otherwise the desk ignores writes unless TakeOwnership has succeeded, or it is blocked")
			}
			return

		case newTarget := <-d.chMove:
			// The same destination again is not a retarget. A consumer
			// republishing a value it already sent is ordinary - a retained
			// message redelivered, a UI echoing its own state - and acting
			// on it would stop a move that is already going where it is
			// asked to go. Worse, stopping near the end leaves a remainder
			// inside the dead band, so the restart reports not moving and
			// the desk ends up short of a target it would have reached.
			if sameDestination(newTarget, mmx10) {
				log.Debug("move already heading there, ignoring repeat",
					"target_tenths_mm", mmx10, "repeat_tenths_mm", newTarget)
				continue
			}

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

			// The burst may have ended where it began. The desk has been
			// stopped by the wait regardless, so the move has to be started
			// again - but it is the same move, not a new one.
			if sameDestination(newTarget, mmx10) {
				log.Debug("move resuming after a repeat", "target_tenths_mm", mmx10)
				movedYet = false
				if !write() {
					return
				}
				resetTimer(stall, stallTimeout)
				continue
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
			// The desk reports only while it moves, so silence here means
			// it never started - this is where a move that goes nowhere
			// ends up, not the speed 0 branch above.
			args := []any{"target_tenths_mm", mmx10}
			if at, ok := d.lastKnownPosition(); ok {
				args = append(args, "last_known_tenths_mm", at, "distance_tenths_mm", abs(mmx10-at))
			}
			args = append(args, "hint", "the desk reports nothing when stationary, so it never moved: either the target is inside its dead band - compare the distance - or writes are being ignored because TakeOwnership has not succeeded")
			log.Warn("move timed out with no position reports", args...)
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

// noteTargetWritten records a height written to ReferenceInput, so the
// next move knows how long ago a different one went out.
func (d *Desk) noteTargetWritten(mmx10 int) {
	d.targetMu.Lock()
	defer d.targetMu.Unlock()
	d.lastTarget, d.haveTarget, d.lastTargetAt = mmx10, true, time.Now()
}

// waitBeforeTarget is how long to leave the controller alone before
// writing this height. Rewriting the same one is what sustains a move and
// needs no wait; a different one is refused until RetargetGap has passed
// since the last write, whether that was this move or the one before.
func (d *Desk) waitBeforeTarget(mmx10 int) time.Duration {
	d.targetMu.Lock()
	defer d.targetMu.Unlock()

	if !d.haveTarget || sameDestination(mmx10, d.lastTarget) {
		return 0
	}
	if wait := RetargetGap - time.Since(d.lastTargetAt); wait > 0 {
		return wait
	}
	return 0
}

// lastKnownPosition is where the desk last reported itself, which may be
// from an earlier move: it says nothing while stationary, so there is no
// fresher answer to be had without moving it.
func (d *Desk) lastKnownPosition() (int, bool) {
	d.targetMu.Lock()
	defer d.targetMu.Unlock()
	return d.lastPosition, d.havePosition
}

// notePosition records a reported height for lastKnownPosition.
func (d *Desk) notePosition(extension int) {
	d.targetMu.Lock()
	defer d.targetMu.Unlock()
	d.lastPosition, d.havePosition = extension, true
}

// ensurePositionTracking starts following the desk's height, once. The
// recorder is deliberately never removed: the desk reports only while it
// moves, so the only way to know where it is between moves is to have
// been listening during the last one - including one somebody made from
// the panel.
func (d *Desk) ensurePositionTracking() {
	if d.roSub == nil {
		return
	}
	d.trackPosition.Do(func() {
		d.roSub.AddReferenceOutputCallback(func(extension, _ int) {
			d.notePosition(extension)
		})
	})
}

// sameDestination reports whether two targets ask for the same place. The
// dead band is the measure: the desk cannot tell apart two heights closer
// than that, so neither should this.
func sameDestination(a, b int) bool { return abs(a-b) <= arrivalTolerance }

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
