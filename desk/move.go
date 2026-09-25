package desk

import (
	"fmt"
	"time"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription/controlerror"
	"github.com/gomi-source/linak-dpg/subscription/referenceoutput"
)

// Moving a DPG desk is not one write. The controller moves only while it
// is being told where to go, so the target is rewritten continuously until
// the desk reports it has arrived. Move itself starts a goroutine and
// returns; every write happens there.
//
// Five things make that harder than it sounds.
//
// The desk only accepts a different height once it has come to rest.
// Rewriting the same height is what sustains movement; stop writing and it
// stops, which is how a retarget brings it to rest - it goes quiet, waits
// for the stream to report speed 0, and only then writes the new height.
// Writing one sooner halts the desk instead of redirecting it. That pause
// belongs to us and is not read as the desk stopping of its own accord.
//
// Going quiet is slow braking. A DPG1M carries on at full speed for about
// 0.85s after the last height and then takes about 0.4s to stop, so a
// retarget at full speed runs on some 40mm. Stop does not shorten it:
// measured by example/moves -scenarios stopcoast, a Stop sent at the moment
// of the reversal left the rest time and the overrun unchanged. Stop
// evidently applies to Control-driven movement, not to a desk heading for
// a height, so a reversal is braked exactly like any other retarget.
//
// A halt is not an arrival, and it is not a thing to push through. The
// desk reports speed 0 both when it has reached the target and when it has
// stopped short - and one reason it stops short is that it has hit
// something. The controller's own safety stop is the only protection a
// person's hand has here, so a halt that is not an arrival ends the move:
// the target is never re-commanded to make a stopped desk try again.
//
// A collision is not even a halt, at first. Measured by example/moves
// -scenarios blocked: a DPG1M descending at full speed into an obstruction
// sent 01 00 3B on the Control error characteristic, reported a stall, and
// then backed away by itself - 32mm in one run, 40mm in another, taking
// 1.6-1.8s - with every report flagged as collision recovery. It ignored
// the heights written to it throughout, but until it came to rest they
// kept coming,
// and a height it did accept once the recovery was over would have sent
// it straight back into what it hit. So an error frame, or a report with
// the recovery flag, ends the move the moment it arrives: nothing more is
// written, and the desk is left to finish its recovery by itself.
//
// The desk reports only while it moves. A stationary one says nothing at
// all, so "no reports" is the normal state between moves and the way a
// move that never started announces itself - there is no speed 0 reading
// to classify, because there is no reading.
//
// Nothing acknowledges a write. ReferenceInput is write-without-response,
// so a target that never reaches the controller looks exactly like one it
// chose to ignore. Once the desk is moving this costs nothing: its reports
// drive the loop, and the next one re-sends the target anyway. From rest
// there are no reports, so a single write that goes missing would end the
// move in silence. That is why the target is offered again, a full
// RetargetGap after the last write, until the desk either starts moving or
// the start window runs out. The spacing is the point: the controller
// ignores a height that arrives too soon after the previous one, and
// restarts that clock on every write, so a retry any sooner could never
// succeed - it would only keep the desk deaf. The retry is for the link,
// not for the desk's decision, and it stops the moment the desk has moved.
// After that a halt is the desk's own and is never re-commanded.
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

	// startGrace is how long the target keeps being offered to a desk that
	// has not begun moving, measured from the first write of that target.
	// Reaching it ends the move: a desk that has not moved by now is not
	// going to.
	//
	// Offers are a RetargetGap apart, so this fits the first write and two
	// more at the default gap. It is deliberately no longer: the retry
	// exists because an unacknowledged write can go missing and a
	// stationary desk reports nothing to notice it by, not because a desk
	// that has declined to move should be talked into it. Once the desk has
	// moved, a later halt is never re-commanded at all.
	startGrace = 2500 * time.Millisecond

	// offerCheck is how often the move looks at whether a re-offer is due.
	// It is only the resolution of that check; the spacing of the offers
	// themselves is RetargetGap, timed from the last write.
	offerCheck = 100 * time.Millisecond

	// progressThreshold is how much the height has to change to count as
	// progress and restart the stall timer, in tenths of a millimetre.
	progressThreshold = 5

	// stopSettle bounds the wait for the desk to come to rest during a
	// retarget. Reaching it is not fatal - the retarget proceeds anyway -
	// so it only stops a retarget hanging forever if the desk goes quiet.
	stopSettle = 3 * time.Second
)

// RetargetGap is the silence the controller needs after one height before
// it will act on the next. Every height Move writes, other than the ones
// sustaining a move already under way, waits for it: the first of a new
// move, the first after a retarget, and each re-offer to a desk that has
// not started.
//
// Measured on a DPG1M by example/moves -scenarios threshold, which brings
// the desk to rest, stays silent for a set time and then writes one height:
// after 500-700ms it was ignored every time (0 of 6), after 800ms half the
// time, and after 900ms to 2.5s never (14 of 14). The direction of either
// move made no difference. The clock appears to run from the last height
// received, and to restart on every one - a desk sent a height too soon
// and then offered it again every 200ms stayed deaf for as long as that
// went on - which is why nothing is re-sent inside the gap either.
//
// This used to be 800ms, which put every move that followed another
// closely on a coin flip: the intermittent failures to move were this.
// A second leaves room for the jitter of a BLE link at the edge, and costs
// nothing on a retarget at full speed, which takes longer than that to
// come to rest anyway.
//
// It is a package variable so a different controller can be given a
// different figure. Change it between moves, not during one.
var RetargetGap = time.Second

// HaltOnRetarget writes the new height the moment a retarget is asked for,
// instead of going quiet and letting the desk coast to rest.
//
// It is an experiment, off by default. Writing a different height to a
// travelling desk halts it rather than redirecting it - which is why a
// retarget normally waits - but a halt may be exactly what a retarget
// wants, if it stops the desk sooner than coasting does: at full speed a
// coast runs on some 40mm. With it on, the retarget still waits for rest
// and for RetargetGap, timed from that halting write, before writing the
// height again to start the new move. example/moves -scenarios stopcoast
// compares the two.
var HaltOnRetarget = false

// reading is one ReferenceOutput report: where the desk is, how fast it
// is going, and whether it is recovering from a collision.
type reading struct {
	extension  int
	speed      int
	recovering bool
}

// faultFrame reports whether a frame from the Control error characteristic
// ends a move. The empty frame does not: it follows an error by about
// 0.8s, whatever the desk is doing, and reads as the error clearing -
// which says nothing about the move. Nor does the code a
// halting write provokes, when this move made that write on purpose -
// expectHalt says so. Anything else does, including codes never seen
// before: when the controller says something has gone wrong and nobody
// knows what, the safe thing is to stop telling it where to go.
func faultFrame(frame []byte, expectHalt bool) bool {
	if controlerror.Cleared(frame) {
		return false
	}
	if code, ok := controlerror.Code(frame); ok && expectHalt && code == controlerror.CodeInterrupted {
		return false
	}
	return true
}

// errorFrames is how many error frames a move holds before dropping more.
// One is enough to end it; the rest are there so an empty frame arriving
// on the heels of a real one cannot push it out, as it could from a slot.
const errorFrames = 8

// Move sends the desk to an extension, in tenths of a millimetre above its
// own base - not above the floor; see BaseOffset for the other frame.
//
// It returns as soon as the move is under way rather than when it
// finishes: a background goroutine writes the target, offers it again if
// the desk has not started, and keeps it in front of the desk while it
// travels. Errors after that are logged, not returned.
//
// Calling Move again while a move is running retargets that move rather
// than starting a second one. Either way round - further on, or back the
// way it came - the desk is brought to rest first and the new height
// written after, since that is the only way it will take one.
//
// A desk that stops short of the target is left stopped. It stops by
// itself when it meets resistance, and telling it to try again would
// defeat the only protection whatever it met has, so the move ends and
// says so. Deciding whether to ask again belongs to the caller, who may
// know the way is clear; this package will not do it silently. A desk
// that reports a collision is not even left stopped: it backs away by
// itself, and the move ends as soon as it says so rather than once it
// has finished - see ControlErrors.
//
// A move to where the desk already is does nothing, and says so rather
// than waiting: the controller has a dead band and will not start for a
// target close to its current position. That is the usual reason a move
// reports not starting - see arrivalTolerance.
//
// Every write is also ignored unless TakeOwnership has succeeded, which is
// the second reason, and the likelier one only if ownership is not being
// asserted on connect.
//
// The third is pairing. A desk that is not paired with this Mac drops
// every height written to it, owner bit or not, and says nothing - the
// characteristic is write-without-response, so there is no reply for a
// refusal to travel in. Everything else works unpaired, which is what
// makes it easy to miss; see the package README.
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
	removeCallback := d.roSub.AddReadingCallback(func(r referenceoutput.Reading) {
		readings.put(reading{extension: r.Extension, speed: r.Speed, recovering: r.Recovering()})
	})

	// The first write happens in the goroutine, not here: the controller
	// may need to be left alone for the rest of RetargetGap before it will
	// accept this height, and Move must not block for that. Neither must it
	// block for the error characteristic, which the first move of all has
	// to discover and subscribe to.
	go func() {
		defer d.moving.Store(false)
		defer removeCallback()

		errs := make(chan []byte, errorFrames)
		if ce, err := d.ControlErrors(); err != nil {
			// Not fatal: the position reports still carry the collision
			// flag, and a blocked desk still stops by itself.
			d.device.Logger().Debug("move is not watching the error characteristic", "err", err)
		} else {
			removeErrors := ce.AddControlErrorCallback(func(frame []byte) {
				select {
				case errs <- frame:
				default: // the move has one already, and one is enough
				}
			})
			defer removeErrors()
		}

		d.runMove(c, readings, errs, mmx10)
	}()

	return nil
}

// runMove sustains the move until the desk arrives, stops, or never
// starts. It writes the target repeatedly *while the desk is moving* -
// which is what keeps it moving - and stops writing the moment it is not.
func (d *Desk) runMove(c dpg.Characteristic, readings *slot[reading], errs <-chan []byte, mmx10 int) {
	log := d.device.Logger()

	var last reading
	haveLast := false
	movedYet := false
	written := time.Now()      // the last write of any height
	offeredSince := time.Now() // the first write of the current one

	stall := time.NewTimer(stallTimeout)
	defer stall.Stop()

	// Until the desk starts moving, this tick decides when the target is
	// offered again - a RetargetGap after the last write; see startGrace.
	// It keeps running afterwards and is ignored, which is cheaper than
	// stopping and restarting it around every retarget.
	offer := time.NewTicker(offerCheck)
	defer offer.Stop()

	write := func() bool {
		if _, err := c.WriteWithoutResponse(target(mmx10)); err != nil {
			log.Warn("move write failed", "target_tenths_mm", mmx10, "err", err)
			return false
		}
		written = time.Now()
		d.noteTargetWritten(mmx10)
		return true
	}

	// start writes the first target of a move or a retarget, and restarts
	// the window during which a desk that has not moved is offered it
	// again.
	start := func() bool {
		offeredSince = time.Now()
		movedYet = false
		if !write() {
			return false
		}
		resetTimer(stall, stallTimeout)
		return true
	}

	// noStart ends a move the desk never began. Two things stop one
	// starting and the distance tells them apart, so it is logged rather
	// than left to be subtracted: a target inside the controller's dead
	// band is simply ignored, which is much the commoner of the two now
	// that ownership is asserted on connect.
	noStart := func() {
		args := []any{"target_tenths_mm", mmx10}
		at, ok := d.lastKnownPosition()
		if haveLast {
			at, ok = last.extension, true
		}
		if ok {
			args = append(args, "at_tenths_mm", at, "distance_tenths_mm", abs(mmx10-at))
		}
		args = append(args, "hint", "a short distance means the target is inside the desk's dead band and it will not move; otherwise the desk is blocked, is not paired with this Mac (it drops heights over an unpaired link, without saying so), or is ignoring writes because TakeOwnership has not succeeded")
		log.Warn("desk did not start moving", args...)
	}

	// fault ends a move the controller has flagged: an error frame, or a
	// position report in collision recovery. Nothing is written after it.
	// The desk carries on with whatever recovery it has started by itself,
	// and that is not this package's to interrupt.
	fault := func(frame []byte, r reading, haveR bool) {
		args := []any{"target_tenths_mm", mmx10}
		if haveR {
			args = append(args, "at_tenths_mm", r.extension)
		}
		msg := "desk reported a collision; move ended, not re-commanding"
		if frame != nil {
			args = append(args, "frame", fmt.Sprintf("% X", frame))
			if code, ok := controlerror.Code(frame); ok {
				args = append(args, "code", fmt.Sprintf("0x%02X", code), "meaning", controlerror.Describe(code))
				if code != controlerror.CodeCollision {
					msg = "desk reported an error; move ended, not re-commanding"
				}
			} else {
				msg = "desk reported an error; move ended, not re-commanding"
			}
		} else {
			args = append(args, "flag", "collision recovery")
		}
		args = append(args, "hint", "after a collision the desk backs away by itself; it ignores heights until it has finished, and a new move before then may not start")
		log.Warn(msg, args...)
	}

	// A move that follows another too closely is the same mistake as a
	// retarget that does: the controller halts rather than redirects. The
	// previous move may have been this Desk's or somebody else's on the same
	// connection, so the last write time is kept across moves rather than
	// per move.
	if wait := d.waitBeforeTarget(mmx10); wait > 0 {
		log.Debug("waiting before a new target", "target_tenths_mm", mmx10, "wait", wait.Round(time.Millisecond))
		time.Sleep(wait)
	}

	if !start() {
		return
	}

	for {
		select {
		case frame := <-errs:
			if !faultFrame(frame, false) {
				continue
			}
			fault(frame, last, haveLast)
			return

		case r := <-readings.ch:
			if haveLast && abs(r.extension-last.extension) >= progressThreshold {
				resetTimer(stall, stallTimeout)
			}
			last, haveLast = r, true

			if r.recovering {
				fault(nil, r, true)
				return
			}

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

			case time.Since(offeredSince) < startGrace:
				// Not started yet. A desk takes a moment to pick up the
				// first target, and the offer tick will put it in front
				// of it again once the gap allows, so there is nothing to
				// do but wait.
				continue

			default:
				noStart()
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
			halting := false
			if HaltOnRetarget {
				// The halting write counts as the last write: the wait below
				// is timed from it, since the controller will ignore the
				// next height if it comes too soon after any height.
				if _, err := c.WriteWithoutResponse(target(newTarget)); err != nil {
					log.Warn("halting write failed", "target_tenths_mm", newTarget, "err", err)
				} else {
					written = time.Now()
					d.noteTargetWritten(newTarget)
					halting = true
				}
			}
			w := waitReadyForNewTarget(readings, errs, halting, last, haveLast, written.Add(RetargetGap))
			last, haveLast = w.last, w.haveLast
			if w.faulted {
				// The desk may have coasted into something - a retarget
				// at full speed runs on for some 40mm.
				fault(w.frame, last, haveLast)
				return
			}

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
				if !start() {
					return
				}
				continue
			}

			// A reversal used to send Stop here. It never fired - the wait
			// above has already brought the desk to rest, and a desk at rest
			// is not reversing - and measured, it would not have helped:
			// Stop does not brake a desk heading for a height. See the note
			// at the top of this file.
			mmx10 = newTarget
			if !start() { // the desk has to start again from rest
				return
			}

		case <-offer.C:
			// Before the desk has moved, a write that went missing is
			// indistinguishable from one it ignored, and nothing will
			// report either. Offer the target again - but only a full gap
			// after the last write, only until the window closes, and
			// never once it has moved.
			if movedYet {
				continue
			}
			if time.Since(offeredSince) >= startGrace {
				noStart()
				return
			}
			if time.Since(written) < RetargetGap {
				continue
			}
			if !write() {
				return
			}

		case <-stall.C:
			// A move that never started is ended by the offer tick long
			// before this, so reaching it means the reports dried up
			// mid-move: the link is gone, or the desk stopped without
			// saying so. Either way there is nothing left to sustain.
			args := []any{"target_tenths_mm", mmx10, "moved", movedYet}
			if at, ok := d.lastKnownPosition(); ok {
				args = append(args, "last_known_tenths_mm", at, "distance_tenths_mm", abs(mmx10-at))
			}
			args = append(args, "hint", "the desk reports while it moves and goes quiet when it stops, so reports ending without a speed 0 usually means the connection dropped")
			log.Warn("move timed out with no position reports", args...)
			return
		}
	}
}

// waitResult is what waitReadyForNewTarget saw.
type waitResult struct {
	last     reading
	haveLast bool

	// faulted is set when the desk flagged a collision or sent an error
	// frame while it was coming to rest. frame is that frame, or nil when
	// it was the recovery flag.
	faulted bool
	frame   []byte
}

// waitReadyForNewTarget writes nothing and waits for the desk to be ready
// for a different height: at rest, and at least gapUntil. Not writing is
// what brings it to rest, so this is the stopping as much as the waiting.
//
// Readings here are not classified as arrivals or stops. The desk coming
// to rest during it is the old move ending - the thing being waited for -
// rather than a stop worth reporting. Faults are another matter: a desk
// that coasts into something on the way to rest says so, and that returns
// at once. halting says the move has just written a halting height, whose
// own error frame is expected and not a fault; see faultFrame.
//
// It gives up after stopSettle so a desk that goes quiet without reporting
// rest cannot hang the move; the retarget then proceeds anyway, which is
// no worse than the fixed pause it replaced.
func waitReadyForNewTarget(readings *slot[reading], errs <-chan []byte, halting bool, last reading, haveLast bool, gapUntil time.Time) waitResult {
	atRest := haveLast && last.speed == 0
	gapPassed := false

	gap := time.NewTimer(time.Until(gapUntil))
	defer gap.Stop()
	deadline := time.NewTimer(stopSettle)
	defer deadline.Stop()

	for {
		if atRest && gapPassed {
			return waitResult{last: last, haveLast: haveLast}
		}
		select {
		case frame := <-errs:
			if faultFrame(frame, halting) {
				return waitResult{last: last, haveLast: haveLast, faulted: true, frame: frame}
			}
		case r := <-readings.ch:
			last, haveLast = r, true
			if r.recovering {
				return waitResult{last: last, haveLast: haveLast, faulted: true}
			}
			atRest = r.speed == 0
		case <-gap.C:
			gapPassed = true
		case <-deadline.C:
			return waitResult{last: last, haveLast: haveLast}
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
