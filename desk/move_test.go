package desk

import (
	"testing"
	"time"
)

// A speed 0 reading is only an arrival if the desk is also near the
// target. Treating any speed 0 as arrival is what made a short first move
// end the whole sequence: the desk reached 4784, reported 0, and the
// retarget to 1484 was written once and then abandoned.
func TestArrivalNeedsPositionNotJustSpeed(t *testing.T) {
	const target = 1484

	arrived := func(r reading) bool {
		return r.speed == 0 && abs(r.extension-target) <= arrivalTolerance
	}

	if arrived(reading{extension: 4784, speed: 0}) {
		t.Error("a halt three metres short counted as arrival")
	}
	if !arrived(reading{extension: target, speed: 0}) {
		t.Error("stopped exactly on target did not count as arrival")
	}
	if !arrived(reading{extension: target + arrivalTolerance, speed: 0}) {
		t.Error("stopped within tolerance did not count as arrival")
	}
	// The tolerance is the dead band, so anything past it is a move the
	// desk would actually perform. Reporting arrival there would end the
	// move before it started.
	if arrived(reading{extension: target + arrivalTolerance + 1, speed: 0}) {
		t.Error("a distance the desk would move for counted as arrival")
	}
	if arrived(reading{extension: target, speed: 12}) {
		t.Error("passing through the target at speed counted as arrival")
	}
}

// The latest reading wins and nothing queues, so a speed 0 recorded before
// a retarget can never surface after it.
func TestReadingsSlotKeepsOnlyTheNewest(t *testing.T) {
	readings := newSlot[reading]()
	readings.put(reading{extension: 4784, speed: 0}) // arrival at the old target
	readings.put(reading{extension: 4700, speed: -30})

	got := <-readings.ch
	if got.speed != -30 {
		t.Fatalf("read %+v, want the newest reading", got)
	}
	select {
	case stale := <-readings.ch:
		t.Fatalf("a stale reading survived: %+v", stale)
	default:
	}
}

func TestNewTargetReplacesAnUnreadOne(t *testing.T) {
	d := &Desk{chMove: make(chan int, 1)}
	d.newTargetHeigh(1000)
	d.newTargetHeigh(2000)

	if got := <-d.chMove; got != 2000 {
		t.Fatalf("queued %d, want the newer target", got)
	}
	select {
	case extra := <-d.chMove:
		t.Fatalf("one target should be pending, also found %d", extra)
	default:
	}
}

// A desk that stops short must be left alone. It stops by itself when it
// meets resistance, so re-commanding the target would drive it back into
// whatever it hit - the case this classification exists to prevent.
func TestHaltClassification(t *testing.T) {
	const target = 5000

	// What runMove decides, in the same order it decides it.
	classify := func(r reading, movedYet bool, sinceWrite time.Duration) string {
		switch {
		case r.speed != 0:
			return "keep going"
		case abs(r.extension-target) <= arrivalTolerance:
			return "arrived"
		case movedYet:
			return "stopped short"
		case sinceWrite < startGrace:
			return "not started yet"
		default:
			return "never started"
		}
	}

	for _, tc := range []struct {
		name       string
		r          reading
		movedYet   bool
		sinceWrite time.Duration
		want       string
	}{
		{"travelling", reading{extension: 3000, speed: 40}, true, 0, "keep going"},
		{"arrived", reading{extension: target, speed: 0}, true, 0, "arrived"},
		{"blocked after moving", reading{extension: 3000, speed: 0}, true, 0, "stopped short"},
		{"blocked long after moving", reading{extension: 3000, speed: 0}, true, 10 * time.Second, "stopped short"},
		{"still picking up the target", reading{extension: 3000, speed: 0}, false, 200 * time.Millisecond, "not started yet"},
		{"deaf or blocked from the start", reading{extension: 3000, speed: 0}, false, startGrace + time.Second, "never started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.r, tc.movedYet, tc.sinceWrite); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A retarget waits for two things: the desk at rest, and the minimum quiet
// period. The desk only accepts a different height once it has stopped,
// and not writing is what stops it - so this is the stopping as much as
// the waiting.
func TestWaitReadyNeedsBothRestAndTheGap(t *testing.T) {
	readings := newSlot[reading]()
	moving := reading{extension: 4900, speed: -30}

	// At rest immediately, but the gap has not passed: must still wait.
	readings.put(reading{extension: 4784, speed: 0})
	start := time.Now()
	last, haveLast := waitReadyForNewTarget(readings, moving, true, start.Add(60*time.Millisecond))

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("returned after %v; the minimum gap was not honoured", elapsed)
	}
	if !haveLast || last.extension != 4784 {
		t.Errorf("last reading %+v, want the one that arrived while waiting", last)
	}
}

// Still moving when the gap expires: keep waiting for rest, because
// writing a height to a desk that has not stopped halts it.
func TestWaitReadyKeepsWaitingWhileStillMoving(t *testing.T) {
	readings := newSlot[reading]()
	go func() {
		time.Sleep(40 * time.Millisecond)
		readings.put(reading{extension: 4800, speed: -30}) // still going
		time.Sleep(40 * time.Millisecond)
		readings.put(reading{extension: 4784, speed: 0}) // now at rest
	}()

	start := time.Now()
	last, _ := waitReadyForNewTarget(readings, reading{extension: 4900, speed: -30}, true,
		start.Add(10*time.Millisecond)) // gap expires almost at once

	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Errorf("returned after %v, before the desk reported rest", elapsed)
	}
	if last.speed != 0 {
		t.Errorf("returned with the desk still moving: %+v", last)
	}
}

// A desk that stops reporting must not hang the move for ever.
func TestWaitReadyGivesUpOnSilence(t *testing.T) {
	readings := newSlot[reading]()
	start := time.Now()
	waitReadyForNewTarget(readings, reading{extension: 4900, speed: -30}, true, start)

	if elapsed := time.Since(start); elapsed < stopSettle {
		t.Errorf("returned after %v, want the full stopSettle backstop", elapsed)
	}
}

// A burst of targets must cost one interruption, not one each. Nudging a
// desk upwards four times in a second is a normal thing for a user to do,
// and each retarget pauses the desk for RetargetGap - so everything that
// arrives during the gap has to collapse into the last one.
func TestLatestTargetCollapsesABurst(t *testing.T) {
	d := &Desk{chMove: make(chan int, 1)}
	for _, v := range []int{1500, 1700, 1900, 2100} {
		d.newTargetHeigh(v)
	}

	got, ok := latestTarget(d.chMove)
	if !ok {
		t.Fatal("no target pending after a burst")
	}
	if got != 2100 {
		t.Errorf("took %d, want the last of the burst", got)
	}
	if _, stillPending := latestTarget(d.chMove); stillPending {
		t.Error("a target survived the drain")
	}
}

func TestLatestTargetOnAnEmptyChannel(t *testing.T) {
	d := &Desk{chMove: make(chan int, 1)}
	if _, ok := latestTarget(d.chMove); ok {
		t.Error("reported a target where none was pending")
	}
}

// The dead band is measured, and the direction of error matters: a
// tolerance above it silently turns real moves into no-ops, because the
// loop reports arrival before the desk has started.
func TestArrivalToleranceMatchesTheMeasuredDeadBand(t *testing.T) {
	const (
		largestIgnored = 12 // 1.2mm: the desk does not move
		smallestMoved  = 13 // 1.3mm: it does
	)

	if arrivalTolerance < largestIgnored {
		t.Errorf("arrivalTolerance = %d, below the dead band: a request of %d would wait for movement the desk will not make",
			arrivalTolerance, largestIgnored)
	}
	if arrivalTolerance >= smallestMoved {
		t.Errorf("arrivalTolerance = %d, at or above the smallest distance the desk moves for (%d): real moves would report arrival before starting",
			arrivalTolerance, smallestMoved)
	}
}

// A consumer republishing a target it already sent is ordinary - a
// retained message redelivered, a UI echoing its own state. Treating that
// as a retarget stops a move that is already going where it is asked to,
// and stopping near the end leaves a remainder inside the dead band, so
// the restart reports not moving and the desk ends up short.
func TestSameDestination(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b int
		same bool
	}{
		{"identical", 7350, 7350, true},
		{"within the dead band", 7350, 7350 + arrivalTolerance, true},
		{"within the dead band, below", 7350, 7350 - arrivalTolerance, true},
		{"just past the dead band", 7350, 7350 + arrivalTolerance + 1, false},
		{"a real move", 7350, 4800, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameDestination(tc.a, tc.b); got != tc.same {
				t.Errorf("sameDestination(%d, %d) = %v, want %v", tc.a, tc.b, got, tc.same)
			}
		})
	}
}

// The controller refuses a different height until it has been left alone,
// and that is as true between two Move calls as within one. A move that
// finishes and is immediately followed by another wrote the new height
// ~370ms after the old one on a DPG1M, and the desk ignored it entirely -
// no movement, and no reports, because it reports only while moving.
func TestWaitBeforeTargetSpansMoves(t *testing.T) {
	d := &Desk{}

	// Nothing written yet: nothing to wait for.
	if wait := d.waitBeforeTarget(4586); wait != 0 {
		t.Errorf("first target waits %v, want none", wait)
	}

	d.noteTargetWritten(4586)

	// The same height again sustains the move and never waits.
	if wait := d.waitBeforeTarget(4586); wait != 0 {
		t.Errorf("repeating a height waits %v, want none", wait)
	}
	if wait := d.waitBeforeTarget(4586 + arrivalTolerance); wait != 0 {
		t.Errorf("a height inside the dead band waits %v, want none", wait)
	}

	// A different one, straight after, waits out what is left of the gap.
	wait := d.waitBeforeTarget(4718)
	if wait <= 0 || wait > RetargetGap {
		t.Errorf("a new height waits %v, want something up to %v", wait, RetargetGap)
	}
}

func TestWaitBeforeTargetExpires(t *testing.T) {
	d := &Desk{}
	d.noteTargetWritten(4586)

	d.targetMu.Lock()
	d.lastTargetAt = time.Now().Add(-2 * RetargetGap)
	d.targetMu.Unlock()

	if wait := d.waitBeforeTarget(4718); wait != 0 {
		t.Errorf("waits %v long after the last write, want none", wait)
	}
}

// A target the desk is already at is a no-op we can recognise, rather
// than five seconds of silence and a warning. The desk reports only while
// it moves, so the position comes from the last time it did.
func TestLastKnownPositionRecognisesANoOp(t *testing.T) {
	d := &Desk{}

	if _, ok := d.lastKnownPosition(); ok {
		t.Error("reported a position before the desk ever did")
	}

	d.notePosition(4586)
	at, ok := d.lastKnownPosition()
	if !ok || at != 4586 {
		t.Fatalf("last known position = %d/%v, want 4586", at, ok)
	}

	if !sameDestination(4586, at) {
		t.Error("the height the desk is at was not recognised as a no-op")
	}
	if sameDestination(4718, at) {
		t.Error("a real move was mistaken for a no-op")
	}
}
