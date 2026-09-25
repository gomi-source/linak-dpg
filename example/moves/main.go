// Command moves drives a desk through movement scenarios and prints
// everything it says while they run: the ReferenceOutput stream, the
// Control service's error characteristic, and the desk package's own log,
// all on one timeline as it happens.
//
// It exists for two things that cannot be had any other way. Move's
// behaviour is the part of this package with no unit test worth the name -
// retargeting, reversing, a burst of targets, a desk that stops short are
// all agreements with a controller, not with a mock - so there has to be
// something that performs them deliberately rather than waiting for them
// to turn up in ordinary use. And the error characteristic, 99FA0003, is
// barely explored: two codes are known - 0x3B for a collision, 0x10 for a
// halting write - so the scenarios here include the states where a frame
// is plausible, and what arrives is printed as the bytes first and the
// reading of them second, with anything of an unseen shape flagged.
//
// Usage:
//
//	go run ./example/moves -list
//	go run ./example/moves -name "DESK 8352" -low 600 -high 3000
//	go run ./example/moves -name "DESK 8352" -scenarios simple,reverse
//	go run ./example/moves -name "DESK 8352" -scenarios blocked
//	go run ./example/moves -name "DESK 8352" -scenarios all
//
// -low and -high are absolute extensions in tenths of a millimetre above
// the desk's own base - not above the floor; see BaseOffset for that other
// frame - and every scenario travels between them. They cannot be derived:
// there is no way to ask a DPG desk where it is, because it reports its
// height only while it is moving. A first run is how to find out what the
// range really is, since the timeline prints every height reported; the
// controller enforces its own limits regardless of what is asked for.
//
// This moves furniture. Clear the desk's path first, and read what a
// scenario does before choosing it - -list prints them. The blocked
// scenario is the one to be careful with: it asks for an obstruction on
// purpose, which means something solid and expendable in the way - a block
// of wood, a full drawer unit - and never a hand, a foot, or a cable worth
// keeping. The desk's own safety stop is what is being exercised, so it is
// also the thing being relied on.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gomi-source/corebluetooth-go/ble"
	"github.com/gomi-source/linak-dpg"
	deskpkg "github.com/gomi-source/linak-dpg/desk"
	"github.com/gomi-source/linak-dpg/subscription/controlerror"
	"github.com/gomi-source/linak-dpg/subscription/referenceoutput"
)

const (
	// moveTimeout bounds a single scenario move. A full travel is a few
	// seconds; this is generous enough that reaching it means something
	// went wrong rather than that the desk is slow.
	moveTimeout = 45 * time.Second

	// arrivalTolerance mirrors the desk package's own dead band, so this
	// tool agrees with it about what counts as having arrived.
	arrivalTolerance = 12

	// trailingQuiet is how long to wait after a scenario for the desk
	// package's own last log line, which trails this tool's verdict by a
	// millisecond or so.
	trailingQuiet = 500 * time.Millisecond
)

type options struct {
	name         string
	peripheralID string
	helperPath   string
	scanTimeout  time.Duration
	low          int
	high         int
	scenarios    string
	settle       time.Duration
	verbose      bool
	list         bool

	gaps      []time.Duration // the threshold scenario's silences, in trial order
	gapRepeat int             // how many times each gap is tried

	stopRepeat      int      // how many trials of each method the stopcoast scenario runs
	reversalMethods []string // which methods it compares, in trial order

	extendRepeat int // how many times the extend scenario runs each of its four trials
}

func main() {
	var o options
	flag.StringVar(&o.name, "name", "", "advertised desk name to connect to (exact, case-insensitive)")
	flag.StringVar(&o.peripheralID, "peripheral-id", "", "CoreBluetooth peripheral UUID, as an alternative to -name")
	flag.StringVar(&o.helperPath, "helper", "", "path to the corebluetoothd binary (default: look in the usual sibling locations)")
	flag.DurationVar(&o.scanTimeout, "scan-timeout", 20*time.Second, "how long to look for the desk")
	flag.IntVar(&o.low, "low", 600, "the lower height scenarios travel to, in tenths of a millimetre above the desk's own base")
	flag.IntVar(&o.high, "high", 3000, "the upper height scenarios travel to, in tenths of a millimetre above the desk's own base")
	flag.StringVar(&o.scenarios, "scenarios", "default", "comma-separated scenario names, or \"default\" for every non-interactive one, or \"all\"")
	flag.DurationVar(&o.settle, "settle", 2*time.Second, "quiet period between scenarios")
	flag.BoolVar(&o.verbose, "v", false, "log the corebluetoothd helper's stderr")
	flag.BoolVar(&o.list, "list", false, "describe the scenarios and exit, without connecting to anything")
	gaps := flag.String("gaps", "500,600,700,800,900,1000,1100,1200,1500,2500", "threshold scenario: the silences to try, in milliseconds")
	flag.IntVar(&o.gapRepeat, "gap-repeat", 2, "threshold scenario: how many times to try each silence")
	flag.IntVar(&o.stopRepeat, "stop-repeat", 3, "stopcoast scenario: how many trials of each method")
	methods := flag.String("reversal-methods", "coast,halt", "stopcoast scenario: the methods to compare, from coast, stop and halt")
	flag.IntVar(&o.extendRepeat, "extend-repeat", 2, "extend scenario: how many times to run each of its four trials")
	flag.Parse()

	var err error
	if o.gaps, err = parseGaps(*gaps); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if o.gapRepeat < 1 {
		fmt.Fprintln(os.Stderr, "-gap-repeat must be at least 1")
		os.Exit(2)
	}
	if o.reversalMethods, err = parseReversalMethods(*methods); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if o.stopRepeat < 1 {
		fmt.Fprintln(os.Stderr, "-stop-repeat must be at least 1")
		os.Exit(2)
	}
	if o.extendRepeat < 1 {
		fmt.Fprintln(os.Stderr, "-extend-repeat must be at least 1")
		os.Exit(2)
	}

	if o.list {
		listScenarios(os.Stdout)
		return
	}
	if o.name == "" && o.peripheralID == "" {
		fmt.Fprintln(os.Stderr, "one of -name or -peripheral-id is required; -list needs neither")
		os.Exit(2)
	}

	selected, err := selectScenarios(o.scenarios)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if o.high-o.low < 300 {
		fmt.Fprintf(os.Stderr, "-low %d and -high %d are %d tenths of a millimetre apart; scenarios need room to travel, so at least 300\n", o.low, o.high, o.high-o.low)
		os.Exit(2)
	}

	if err := run(o, selected); err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		os.Exit(1)
	}
}

func run(o options, selected []scenario) error {
	ctx := context.Background()

	helperPath := o.helperPath
	if helperPath == "" {
		if found := findHelper(); found != "" {
			helperPath = found
			fmt.Printf("using helper at %s\n", helperPath)
		}
	}

	var stderr *os.File
	if o.verbose {
		stderr = os.Stderr
	}
	client, err := ble.Start(ctx, ble.Options{HelperPath: helperPath, Stderr: stderr})
	if err != nil {
		return fmt.Errorf("starting CoreBluetooth helper: %w\n\n%s", err, helperHint())
	}
	defer client.Close()

	peripheralID := o.peripheralID
	if peripheralID == "" {
		peripheralID, err = findDesk(ctx, client, o.name, o.scanTimeout)
		if err != nil {
			return err
		}
	}

	fmt.Printf("connecting to %s\n", peripheralID)
	if err := client.Connect(ctx, peripheralID, 30*time.Second); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.Disconnect(dctx, peripheralID)
		cancel()
	}()

	rec := newRecorder(os.Stdout)

	// Deferred after the Disconnect above, so it runs first: the
	// disconnection that follows is this tool's own, and should not read
	// like the desk dropping the link.
	defer rec.markClosing()

	// Everything the desk package would log goes onto the same timeline as
	// the frames, which is the whole point: "move retargeting" three
	// milliseconds before a speed drops to 0 is the sort of thing that is
	// invisible in two separate streams.
	device := dpg.NewDevice(client, peripheralID).WithLogger(rec.logger())
	d := deskpkg.New(device, o.name)

	r := &runner{o: o, d: d, rec: rec, readings: make(chan reading, 256), errs: make(chan []byte, 64)}

	d.Positions().AddReadingCallback(func(rd referenceoutput.Reading) {
		r.note(rd.Extension, rd.Speed)
		rec.position(rd)
		select {
		case r.readings <- reading{at: time.Now(), extension: rd.Extension, speed: rd.Speed, flags: rd.Flags}:
		default: // the waiter has gone; the newest reading is the one that matters
		}
	})

	// A desk without this characteristic, or a helper that will not
	// discover it, is not a reason to stop: every scenario still runs, and
	// the report says the frames were never being watched for.
	//
	// It is the Desk's own subscription, the one Move watches, rather
	// than a second dispatcher on the same characteristic.
	if ce, err := d.ControlErrors(); err != nil {
		fmt.Printf("\nno subscription to the error characteristic (%v)\n"+
			"scenarios will run, but nothing will be recorded from 99FA0003\n", err)
	} else {
		ce.AddControlErrorCallback(rec.controlError)
		ce.AddControlErrorCallback(func(frame []byte) {
			select {
			case r.errs <- frame:
			default: // nobody is collecting them; the timeline has them anyway
			}
		})
		r.watchingErrors = true
	}

	// Every write is ignored without the owner bit, including the ones
	// that make a desk move - and it is ignored silently, which looks
	// exactly like a desk that decided not to move. Establishing it up
	// front removes that explanation from everything below.
	changed, err := d.TakeOwnership(ctx)
	if err != nil {
		return fmt.Errorf("taking ownership: %w", err)
	}
	if changed {
		fmt.Println("owner bit was not set; it is now")
	} else {
		fmt.Println("owner bit already set")
	}

	watchDisconnections(client, peripheralID, rec)

	fmt.Printf("\ntravelling between %d and %d (tenths of a millimetre above the desk's own base)\n", o.low, o.high)
	fmt.Println("stand clear of the desk")
	for i, s := range selected {
		if i > 0 {
			time.Sleep(o.settle)
		}
		rec.begin(s.name, s.desc)
		err := s.run(r)
		if err != nil {
			rec.note(fmt.Sprintf("scenario failed: %v", err))
		}
		// The desk package's move goroutine sees the same final reading
		// this tool does and logs its verdict a moment later. Waiting for
		// it keeps that line inside the scenario it belongs to, instead of
		// after the next heading - or after the summary.
		time.Sleep(trailingQuiet)
		rec.endScenario()
		if err != nil {
			return err
		}
	}

	rec.reportErrors(r.watchingErrors)
	return nil
}

// ---------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------

type scenario struct {
	name        string
	desc        string
	interactive bool
	optIn       string // why it is left out of the default set, when it is for a reason other than being interactive
	run         func(*runner) error
}

var scenarios = []scenario{
	{
		name: "simple",
		desc: "two complete moves in a row, low then high - the shape that used to fail silently, when a lost write from rest had nothing to retry it",
		run: func(r *runner) error {
			if err := r.moveAndSettle(r.o.low); err != nil {
				return err
			}
			r.pause(2 * time.Second)
			return r.moveAndSettle(r.o.high)
		},
	},
	{
		name: "repeat",
		desc: "the same target sent twice more while the move is running - a consumer republishing a retained value, which must not interrupt the move",
		run: func(r *runner) error {
			if err := r.moveAndSettle(r.o.low); err != nil {
				return err
			}
			r.pause(time.Second)
			if err := r.moveTo(r.o.high); err != nil {
				return err
			}
			if !r.waitMoving(5 * time.Second) {
				r.note2("never started, so there is nothing to repeat at")
			}
			for i := 0; i < 2; i++ {
				time.Sleep(400 * time.Millisecond)
				if err := r.moveTo(r.o.high); err != nil {
					return err
				}
			}
			r.settleAt(r.o.high)
			return nil
		},
	},
	{
		name: "retarget",
		desc: "a nearer target written while the desk is travelling the same way - it has to stop, wait out RetargetGap, and start again",
		run: func(r *runner) error {
			if err := r.moveAndSettle(r.o.low); err != nil {
				return err
			}
			r.pause(time.Second)
			if err := r.moveTo(r.o.high); err != nil {
				return err
			}
			if !r.waitMoving(5 * time.Second) {
				return fmt.Errorf("the desk never started, so there was nothing to retarget")
			}
			time.Sleep(1200 * time.Millisecond)
			mid := r.o.low + (r.o.high-r.o.low)/2
			if err := r.moveTo(mid); err != nil {
				return err
			}
			r.settleAt(mid)
			return nil
		},
	},
	{
		name: "reverse",
		desc: "a target back the way the desk came, written while it is travelling",
		run: func(r *runner) error {
			if err := r.moveAndSettle(r.o.low); err != nil {
				return err
			}
			r.pause(time.Second)
			if err := r.moveTo(r.o.high); err != nil {
				return err
			}
			if !r.waitMoving(5 * time.Second) {
				return fmt.Errorf("the desk never started, so there was nothing to reverse")
			}
			time.Sleep(1500 * time.Millisecond)
			if err := r.moveTo(r.o.low); err != nil {
				return err
			}
			r.settleAt(r.o.low)
			return nil
		},
	},
	{
		name: "burst",
		desc: "four targets 150ms apart, climbing - they should coalesce into one interruption, not four",
		run: func(r *runner) error {
			if err := r.moveAndSettle(r.o.low); err != nil {
				return err
			}
			r.pause(time.Second)
			span := r.o.high - r.o.low
			var last int
			for i := 1; i <= 4; i++ {
				last = r.o.low + span*i/4
				if err := r.moveTo(last); err != nil {
					return err
				}
				time.Sleep(150 * time.Millisecond)
			}
			r.settleAt(last)
			return nil
		},
	},
	{
		name: "deadband",
		desc: "a target 0.8mm away and then one 2mm away - the first is inside the controller's dead band and must be recognised as a no-op rather than waited on",
		run: func(r *runner) error {
			at, ok := r.lastKnown()
			if !ok {
				r.note2("no height reported yet, so moving once to find out where the desk is")
				if err := r.moveAndSettle(r.o.high); err != nil {
					return err
				}
				if at, ok = r.lastKnown(); !ok {
					return fmt.Errorf("the desk reported no height at all, so there is nothing to measure from")
				}
			}
			r.note2(fmt.Sprintf("measuring from %d", at))

			r.note2("8 tenths away - inside the dead band, expect no move and an immediate answer")
			if err := r.moveTo(at - 8); err != nil {
				return err
			}
			r.settleAt(at - 8)

			r.pause(time.Second)
			r.note2("20 tenths away - outside it, expect a short move")
			if err := r.moveTo(at - 20); err != nil {
				return err
			}
			r.settleAt(at - 20)
			return nil
		},
	},
	{
		name: "stop",
		desc: "Stop sent in the middle of a move - what the desk reports when it is halted deliberately, for comparison with being blocked",
		run: func(r *runner) error {
			if err := r.moveAndSettle(r.o.low); err != nil {
				return err
			}
			r.pause(time.Second)
			if err := r.moveTo(r.o.high); err != nil {
				return err
			}
			if !r.waitMoving(5 * time.Second) {
				return fmt.Errorf("the desk never started, so there was nothing to stop")
			}
			time.Sleep(1500 * time.Millisecond)
			r.act("Stop()")
			if err := r.d.Stop(); err != nil {
				return err
			}
			r.settleAt(r.o.high)
			return nil
		},
	},
	{
		name:        "blocked",
		desc:        "a long move into an obstruction you put there - the desk's own safety stop, and the likeliest thing to make the error characteristic speak",
		interactive: true,
		run: func(r *runner) error {
			fmt.Println()
			fmt.Println("  This scenario drives the desk into whatever is in its way.")
			fmt.Println("  Put something solid and expendable under it - a block of wood, a")
			fmt.Println("  full drawer unit. Never a hand, a foot, or a cable worth keeping.")
			fmt.Println()
			fmt.Printf("  The desk will first rise to %d, then come down to %d.\n", r.o.high, r.o.low)
			fmt.Print("  Press Enter when the way up is clear, or Ctrl-C to abandon this: ")
			bufio.NewScanner(os.Stdin).Scan()

			if err := r.moveAndSettle(r.o.high); err != nil {
				return err
			}

			fmt.Println()
			fmt.Print("  Now place the obstruction, stand clear, and press Enter: ")
			bufio.NewScanner(os.Stdin).Scan()

			r.note2("descending into the obstruction")
			if err := r.moveTo(r.o.low); err != nil {
				return err
			}
			r.settleAt(r.o.low)

			// Move ends at the collision; the desk does not. It backs away
			// by itself, and whether the error characteristic then clears
			// is one of the things this run is for - so keep watching
			// until the desk has been quiet a while.
			r.note2("move over; watching the desk recover, and for the error to clear")
			r.watchUntilQuiet(blockedQuiet)
			return nil
		},
	},
	{
		name:  "threshold",
		desc:  "how long the desk must hear nothing after arriving before it will take a new height - each trial arrives, stays silent for one of -gaps, writes a single height directly, and records whether the desk moved",
		optIn: "takes several minutes",
		run:   runThreshold,
	},
	{
		name:  "stopcoast",
		desc:  "a reversal at full speed, braked by each of -reversal-methods in turn - coasting, Stop, or halting with the new height at once - and how long each takes to come to rest, how far it overruns, and how soon it heads back",
		optIn: "takes a minute or two",
		run:   runStopCoast,
	},
	{
		name:  "extend",
		desc:  "a new height in the same direction written to a desk at full speed, bypassing Move - does it follow without stopping, halt as a reversal does, or ignore it? Four trials: further and nearer, up and down. Needs -low and -high at least 100mm apart",
		optIn: "takes a couple of minutes",
		run:   runExtend,
	},
}

func selectScenarios(spec string) ([]scenario, error) {
	byName := make(map[string]scenario, len(scenarios))
	for _, s := range scenarios {
		byName[s.name] = s
	}

	switch strings.TrimSpace(spec) {
	case "all":
		return scenarios, nil
	case "", "default":
		var out []scenario
		for _, s := range scenarios {
			if !s.interactive && s.optIn == "" {
				out = append(out, s)
			}
		}
		return out, nil
	}

	var out []scenario
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		s, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("no scenario named %q; -list shows them all", name)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no scenarios selected")
	}
	return out, nil
}

func listScenarios(w *os.File) {
	fmt.Fprintln(w, "Scenarios, in the order -scenarios all runs them:")
	fmt.Fprintln(w)
	for _, s := range scenarios {
		heading := s.name
		switch {
		case s.interactive:
			heading = fmt.Sprintf("%-9s  (interactive; not in the default set)", s.name)
		case s.optIn != "":
			heading = fmt.Sprintf("%-9s  (%s; not in the default set)", s.name, s.optIn)
		}
		fmt.Fprintf(w, "  %s\n", heading)
		fmt.Fprintf(w, "    %s\n\n", wrap(s.desc, 72, "    "))
	}
	fmt.Fprintln(w, "Every one of them moves the desk. -low and -high say how far.")
}

// ---------------------------------------------------------------------
// Driving the desk
// ---------------------------------------------------------------------

type reading struct {
	at        time.Time
	extension int
	speed     int
	flags     uint8
}

type runner struct {
	o        options
	d        *deskpkg.Desk
	rec      *recorder
	readings chan reading
	errs     chan []byte // error frames, for scenarios that tally them per trial

	watchingErrors bool

	mu       sync.Mutex
	lastPos  int
	havePos  bool
	lastSeen time.Time // the most recent report of any kind
	lastRest time.Time // the most recent report of speed 0
	lastDir  int       // the sign of the most recent non-zero speed: 1 up, -1 down
}

func (r *runner) note(extension, speed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.lastPos, r.havePos = extension, true
	r.lastSeen = now
	if speed == 0 {
		r.lastRest = now
	} else {
		r.lastDir = sign(speed)
	}
}

// motion reports when the desk last said anything, when it last reported
// standing still, and which way it was last moving.
func (r *runner) motion() (seen, rest time.Time, dir int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSeen, r.lastRest, r.lastDir
}

func (r *runner) lastKnown() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastPos, r.havePos
}

func (r *runner) act(text string)   { r.rec.act(text) }
func (r *runner) note2(text string) { r.rec.note(text) }

func (r *runner) pause(d time.Duration) {
	r.rec.note(fmt.Sprintf("waiting %v", d))
	time.Sleep(d)
}

// moveTo issues a target and returns as soon as Move has taken it. Move
// returns before the desk has done anything, which is exactly the property
// the mid-move scenarios need.
func (r *runner) moveTo(target int) error {
	r.rec.act(fmt.Sprintf("Move(%d)", target))
	return r.d.Move(target)
}

// moveAndSettle is the whole of an uncomplicated move: ask, then wait it
// out and say how it ended.
func (r *runner) moveAndSettle(target int) error {
	if err := r.moveTo(target); err != nil {
		return err
	}
	r.settleAt(target)
	return nil
}

// settleAt waits for the move to be over and records how it ended. It
// never fails: a move that does not happen is a result, and the scenario
// after this one is allowed to run.
//
// "Over" is the desk package's verdict, read from Desk.Moving, not the
// first speed 0 in the stream. A retarget brings the desk to rest on the
// way - that is how it changes height - so a speed 0 is not an ending.
// Treating it as one ended scenarios early, started the next one under a
// desk that was still travelling, and measured the dead band from a height
// the desk had already left. Waiting on Moving also ends a move that never
// starts when the package gives up on it, rather than at this tool's own
// timeout, which is what made a failed start look like a hang.
func (r *runner) settleAt(target int) {
	start := time.Now()
	deadline := start.Add(moveTimeout)
	var last reading
	reported := false

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for r.d.Moving() {
		if time.Now().After(deadline) {
			r.rec.note(fmt.Sprintf("gave up after %v: the move was still running", moveTimeout))
			return
		}
		select {
		case rd := <-r.readings:
			last, reported = rd, true
		case <-tick.C:
		}
	}
	if rd, ok := r.newestReading(); ok {
		last, reported = rd, true
	}
	took := time.Since(start).Round(time.Millisecond)

	switch {
	case reported:
		r.verdict(target, last.extension, fmt.Sprintf("move over after %v", took))
	case took < 100*time.Millisecond:
		// Move returned without starting anything: it judged the target
		// to be where the desk already is.
		if at, ok := r.lastKnown(); ok {
			r.rec.note(fmt.Sprintf("no move: Move treated %d as a no-op, %d from the last known height %d",
				target, abs(target-at), at))
		} else {
			r.rec.note(fmt.Sprintf("no move: Move treated %d as a no-op", target))
		}
	default:
		if at, ok := r.lastKnown(); ok {
			r.rec.note(fmt.Sprintf("no reports at all after %v: the desk never moved, and was last seen at %d, %d away from %d",
				took, at, abs(target-at), target))
		} else {
			r.rec.note(fmt.Sprintf("no reports at all after %v: the desk never moved, and has never reported a height", took))
		}
	}
}

// newestReading empties the readings channel and returns the last one in
// it, so what arrived while a move was ending is not left behind to be
// read as the start of the next.
// blockedQuiet is how long the blocked scenario keeps watching after the
// desk's last report. The empty frame has followed an error by about 0.8s,
// and has once not come at all; this leaves plenty of room to tell which.
const blockedQuiet = 3 * time.Second

// watchUntilQuiet waits until the desk has reported nothing for quiet,
// and says where it came to rest. Frames keep printing on the timeline
// meanwhile.
func (r *runner) watchUntilQuiet(quiet time.Duration) {
	start := time.Now()
	var last reading
	seen := false
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	for {
		select {
		case rd := <-r.readings:
			last, seen = rd, true
			resetTimer(timer, quiet)
		case <-timer.C:
			if !seen {
				r.rec.note(fmt.Sprintf("nothing reported in %v", quiet))
				return
			}
			r.rec.note(fmt.Sprintf("at rest at %d, %v after the move ended; flags at the end 0x%X",
				last.extension, last.at.Sub(start).Round(time.Millisecond), last.flags))
			return
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

func (r *runner) newestReading() (reading, bool) {
	var last reading
	found := false
	for {
		select {
		case rd := <-r.readings:
			last, found = rd, true
		default:
			return last, found
		}
	}
}

// verdict says how a move ended in the terms the desk package uses, so a
// timeline can be read against its log lines.
func (r *runner) verdict(target, at int, how string) {
	short := abs(target - at)
	switch {
	case short <= arrivalTolerance:
		r.rec.note(fmt.Sprintf("arrived at %d (%s)", at, how))
	default:
		r.rec.note(fmt.Sprintf("stopped at %d, %d short of %d (%s)", at, short, target, how))
	}
}

// waitMoving waits for the desk to report that it is actually travelling,
// which is what the mid-move scenarios have to see before they interfere.
func (r *runner) waitMoving(timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case rd := <-r.readings:
			if rd.speed != 0 {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// ---------------------------------------------------------------------
// Recording
// ---------------------------------------------------------------------

// recorder prints one timeline as it happens - the desk's frames, the desk
// package's log, and this tool's own actions - and keeps the error frames
// for a summary at the end.
//
// Printing live rather than at the end matters here: somebody is standing
// next to a moving desk deciding whether to catch it.
type recorder struct {
	mu    sync.Mutex
	out   *os.File
	start time.Time

	scenario string
	ran      []string // scenario names, in the order they ran
	closing  bool     // set once this tool is disconnecting on purpose

	lastInputWrite time.Time // when a height last went to ReferenceInput
	positions      int
	errFrames      int
	errCounts      map[string]int
	errWhere       map[string]map[string]bool
	errPayload     map[string][]byte
}

func newRecorder(out *os.File) *recorder {
	return &recorder{
		out:        out,
		start:      time.Now(),
		errCounts:  make(map[string]int),
		errWhere:   make(map[string]map[string]bool),
		errPayload: make(map[string][]byte),
	}
}

func (r *recorder) begin(name, desc string) {
	r.mu.Lock()
	r.scenario = name
	r.ran = append(r.ran, name)
	r.start = time.Now()
	r.positions = 0
	r.errFrames = 0
	out := r.out
	r.mu.Unlock()

	fmt.Fprintf(out, "\n\n=== %s ===\n%s\n\n", name, wrap(desc, 72, ""))
}

func (r *recorder) endScenario() {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "\n    %d position frames, %d error frames\n", r.positions, r.errFrames)
}

// printf writes free-form lines - a table, a heading - under the same
// lock as the timeline, so they cannot be split by a frame arriving.
func (r *recorder) printf(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, format, a...)
}

// noteInputWrite records that a height has just been written to
// ReferenceInput, by anyone.
func (r *recorder) noteInputWrite() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastInputWrite = time.Now()
}

// lastWrite is when a height last went to ReferenceInput.
func (r *recorder) lastWrite() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastInputWrite
}

func (r *recorder) markClosing() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closing = true
}

func (r *recorder) isClosing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closing
}

func (r *recorder) emit(kind, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "%9s  %-3s  %s\n", fmt.Sprintf("+%.3fs", time.Since(r.start).Seconds()), kind, text)
}

func (r *recorder) act(text string)  { r.emit(">>", text) }
func (r *recorder) note(text string) { r.emit("--", text) }

func (r *recorder) position(rd referenceoutput.Reading) {
	r.mu.Lock()
	r.positions++
	r.mu.Unlock()
	text := fmt.Sprintf("%5d  speed %d", rd.Extension, rd.Speed)
	if rd.Flags != 0 {
		text += fmt.Sprintf("  flags 0x%X", rd.Flags)
		if rd.Recovering() {
			text += " (collision recovery)"
		}
	}
	r.emit("POS", text)
}

// controlError records a frame from 99FA0003: the bytes as they arrived,
// then what they are taken to mean beside them rather than in place of
// them.
func (r *recorder) controlError(data []byte) {
	key := hexOf(data)

	r.mu.Lock()
	r.errFrames++
	r.errCounts[key]++
	if r.errWhere[key] == nil {
		r.errWhere[key] = make(map[string]bool)
	}
	r.errWhere[key][r.scenario] = true
	r.errPayload[key] = data
	r.mu.Unlock()

	r.emit("ERR", fmt.Sprintf("%s   %s", key, readings(data)))
}

// reportErrors is the point of the exercise: every distinct frame the
// error characteristic produced, and which scenarios produced it.
func (r *recorder) reportErrors(watching bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.out, "\n\n=== control error frames (99FA0003) ===\n\n")
	if !watching {
		fmt.Fprintln(r.out, "    nothing was watching the characteristic on this run")
		return
	}
	if len(r.errCounts) == 0 {
		// A result, but only for what actually ran: silence through a
		// deliberate Stop says nothing about a desk stopped by an
		// obstruction.
		fmt.Fprintln(r.out, "    none - the characteristic said nothing during:")
		fmt.Fprintf(r.out, "    %s\n", wrap(strings.Join(r.ran, ", "), 68, "    "))
		if !contains(r.ran, "blocked") {
			fmt.Fprintln(r.out, "    blocked did not run, so this says nothing yet about a desk")
			fmt.Fprintln(r.out, "    stopped by an obstruction - that needs -scenarios blocked")
		}
		return
	}

	keys := make([]string, 0, len(r.errCounts))
	for k := range r.errCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		where := make([]string, 0, len(r.errWhere[k]))
		for s := range r.errWhere[k] {
			where = append(where, s)
		}
		sort.Strings(where)
		fmt.Fprintf(r.out, "    %-24s x%-3d  %s\n", k, r.errCounts[k], strings.Join(where, ", "))
		fmt.Fprintf(r.out, "    %-24s       %s\n\n", "", readings(r.errPayload[k]))
	}
}

// logger routes the desk package's own log onto this timeline. slog's text
// handler does the formatting; the time is dropped because the timeline
// already carries one, relative to the scenario rather than the wall
// clock.
func (r *recorder) logger() *slog.Logger {
	h := slog.NewTextHandler(lineWriter{r}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(h)
}

type lineWriter struct{ rec *recorder }

func (w lineWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	// The desk package logs every height it writes, as it writes it. That
	// makes its log the one place that knows when the last height went out
	// - including the writes a move makes on its own goroutine - which the
	// threshold scenario measures its silences from.
	if strings.Contains(line, `msg="gatt write" characteristic=99FA0031`) {
		w.rec.noteInputWrite()
	}
	w.rec.emit("log", line)
	return len(p), nil
}

func watchDisconnections(client *ble.Client, peripheralID string, rec *recorder) {
	// A DPG controller drops the link on its own now and then. Without
	// noticing it, the symptom is a scenario in which nothing happens -
	// indistinguishable from a desk that ignored the write.
	go func() {
		for d := range client.Disconnections() {
			if !strings.EqualFold(d.PeripheralID, peripheralID) {
				continue
			}
			switch {
			case rec.isClosing():
				rec.note("disconnected, as this tool asked")
			case d.Err != nil:
				rec.note(fmt.Sprintf("the desk disconnected: %v", d.Err))
			default:
				rec.note("the desk disconnected")
			}
		}
	}()
}

// ---------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------

func hexOf(b []byte) string {
	if len(b) == 0 {
		return "(empty)"
	}
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, " ")
}

// readings says what a frame is taken to mean. Every frame seen so far has
// been 01 00 <code> or empty; anything shaped otherwise is flagged as new
// rather than forced into that reading.
func readings(b []byte) string {
	if controlerror.Cleared(b) {
		return "empty - taken as the error clearing"
	}
	code, ok := controlerror.Code(b)
	if !ok {
		return fmt.Sprintf("%d bytes - a shape not seen before", len(b))
	}
	out := fmt.Sprintf("code 0x%02X: %s", code, controlerror.Describe(code))
	if b[0] != 0x01 || b[1] != 0x00 || len(b) != 3 {
		out += fmt.Sprintf(" (and %d bytes not starting 01 00 - not seen before)", len(b))
	}
	return out
}

func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			b.WriteString(line + "\n" + indent)
			line = w
			continue
		}
		line += " " + w
	}
	b.WriteString(line)
	return b.String()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---------------------------------------------------------------------
// Connecting
// ---------------------------------------------------------------------

// Kept in step with example/frames, which explains why the list looks like
// this: a Homebrew install is found by ble.Start's own $PATH lookup
// anyway, but `go run` and a bare .app bundle are not.
var helperCandidates = []string{
	"bin/corebluetoothd.app/Contents/MacOS/corebluetoothd",
	"/opt/homebrew/bin/corebluetoothd", // brew, Apple Silicon prefix
	"/usr/local/bin/corebluetoothd",    // brew, Intel prefix
	"../corebluetooth-go/helper/.build/corebluetoothd.app/Contents/MacOS/corebluetoothd",
	"../corebluetooth-go/bin/corebluetoothd.app/Contents/MacOS/corebluetoothd",
	"../mqtt-linak/bin/corebluetoothd.app/Contents/MacOS/corebluetoothd",
}

func findHelper() string {
	for _, c := range helperCandidates {
		fi, err := os.Stat(c)
		if err != nil || fi.IsDir() {
			continue
		}
		abs, err := filepath.Abs(c)
		if err != nil {
			return c
		}
		return abs
	}
	return ""
}

func helperHint() string {
	var b strings.Builder
	b.WriteString("The corebluetoothd helper was not found. Get it one of these ways:\n\n")
	b.WriteString("    brew tap gomi-source/corebluetooth-go\n")
	b.WriteString("    brew trust gomi-source/corebluetooth-go   # once, Homebrew >= 6.0\n")
	b.WriteString("    brew install corebluetoothd\n\n")
	b.WriteString("or download corebluetoothd.app from a release:\n\n")
	b.WriteString("    https://github.com/gomi-source/corebluetooth-go/releases/latest\n\n")
	b.WriteString("or, building corebluetooth-go locally as a sibling checkout:\n\n")
	b.WriteString("    make -C ../corebluetooth-go helper\n\n")
	b.WriteString("and it will be picked up automatically. Looked in:\n")
	for _, c := range helperCandidates {
		b.WriteString("    " + c + "\n")
	}
	b.WriteString("\nOtherwise pass -helper with the path to the executable *inside* the\n")
	b.WriteString("bundle, not the bundle itself.")
	return b.String()
}

func findDesk(ctx context.Context, client *ble.Client, name string, timeout time.Duration) (string, error) {
	fmt.Printf("scanning for %q...\n", name)
	if err := client.StartScan(ctx, ble.ScanOptions{}); err != nil {
		return "", fmt.Errorf("starting scan: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.StopScan(sctx)
		cancel()
	}()

	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			return "", fmt.Errorf("no peripheral named %q found in %v", name, timeout)
		case p, ok := <-client.Discoveries():
			if !ok {
				return "", fmt.Errorf("discovery channel closed")
			}
			for _, got := range []*string{p.Name, p.AdvertisementData.LocalName} {
				if got != nil && strings.EqualFold(strings.TrimSpace(*got), strings.TrimSpace(name)) {
					fmt.Printf("found %q at %s (rssi %d)\n", *got, p.PeripheralID, p.RSSI)
					return p.PeripheralID, nil
				}
			}
		}
	}
}
