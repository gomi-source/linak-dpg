// Command frames records every raw notification the DeskPanel
// characteristic emits, labelled with the request that provoked it. It is
// how the open questions in this package get settled: ask the desk, keep
// the bytes, and write down what came back.
//
// It deliberately does not use dpg's subscription machinery: that routes
// by message type and drops short frames, which is exactly the behaviour
// worth watching. Notifications are read straight off the ble client.
// -write-reminder is the one exception, because there the typed API is
// what is being tested.
//
// Usage:
//
//	go run ./example/frames -name "DESK 8352"
//	go run ./example/frames -name "DESK 8352" -writes
//	go run ./example/frames -name "DESK 8352" -owner
//	go run ./example/frames -name "DESK 8352" -set-memory 2=4800
//	go run ./example/frames -name "DESK 8352" -unset-memory 1
//	go run ./example/frames -name "DESK 8352" -probe-reminder
//	go run ./example/frames -name "DESK 8352" -write-reminder
//	go run ./example/frames -name "DESK 8352" -listen 2m
//	go run ./example/frames -name "DESK 8352" -hold 10m
//
// Without a flag it only reads, which is always safe. Each mode below
// writes to the desk and restores afterwards, printing the original values
// so they can be put back by hand if a dropped connection leaves the
// restore unapplied.
//
//   - -set-memory / -unset-memory write a memory button. -unset-memory
//     captures the current height first; the frame of interest is the read
//     between clearing and restoring.
//   - -probe-reminder writes the reminder settings back unchanged, once
//     with an extension byte and once without, and reports which form the
//     desk parsed. This is what established that the extension byte
//     belongs to specific commands rather than to writes in general.
//   - -write-reminder is its counterpart in the write direction, going
//     through desk.WriteReminder rather than bytes built here. It asserts
//     the owner bit first, since a write without it is ignored in a way
//     that looks just like a rejected one.
//
// -listen and -hold both sit quiet afterwards and write nothing. -listen
// reports any frame the desk sends unprompted, which is how to find out
// whether DeskPanel is purely request/response; -hold reports if and when
// the desk drops the link, which is the only way to measure that.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gomi-source/corebluetooth-go/ble"
	"github.com/gomi-source/linak-dpg"
	deskpkg "github.com/gomi-source/linak-dpg/desk"
)

func main() {
	var (
		name          = flag.String("name", "", "advertised desk name to connect to (exact, case-insensitive)")
		peripheralID  = flag.String("peripheral-id", "", "CoreBluetooth peripheral UUID, as an alternative to -name")
		helperPath    = flag.String("helper", "", "path to the corebluetoothd binary (default: look in the usual sibling locations)")
		scanTimeout   = flag.Duration("scan-timeout", 20*time.Second, "how long to look for the desk")
		settle        = flag.Duration("settle", 1500*time.Millisecond, "quiet period after each request, for late frames to arrive")
		doWrites      = flag.Bool("writes", false, "also probe writes, by writing back the values just read (no net change)")
		asOwner       = flag.Bool("owner", false, "set the owner bit before reading, then repeat the memory reads, to test whether reads need ownership")
		listen        = flag.Duration("listen", 0, "after the requests, sit idle and report any DeskPanel frames the desk sends unprompted - store a favourite from the panel while this runs")
		hold          = flag.Duration("hold", 0, "after the requests, stay connected and idle for this long, reporting if and when the desk drops")
		setMemory     = flag.String("set-memory", "", "write a memory position, as N=tenths-of-a-millimetre (e.g. 2=4800)")
		unsetMemory   = flag.Int("unset-memory", 0, "clear memory position 1-4, read it, then write its previous value back (writes to the desk, but restores)")
		probeReminder = flag.Bool("probe-reminder", false, "write the reminder settings back unchanged, both without and with an extension byte, to find out whether that byte is height-specific (writes to the desk, but restores)")
		writeReminder = flag.Bool("write-reminder", false, "exercise desk.WriteReminder: set preset 1 to a lopsided value through the typed API, read it back, then restore (writes to the desk, but restores)")
		verbose       = flag.Bool("v", false, "log the corebluetoothd helper's stderr")
	)
	flag.Parse()

	if *name == "" && *peripheralID == "" {
		fmt.Fprintln(os.Stderr, "one of -name or -peripheral-id is required")
		flag.Usage()
		os.Exit(2)
	}
	if *unsetMemory < 0 || *unsetMemory > 4 {
		fmt.Fprintln(os.Stderr, "-unset-memory must be between 1 and 4, or 0 to skip")
		os.Exit(2)
	}

	opts := options{
		name: *name, peripheralID: *peripheralID, helperPath: *helperPath,
		scanTimeout: *scanTimeout, settle: *settle,
		doWrites: *doWrites, asOwner: *asOwner, hold: *hold, listen: *listen,
		setMemory: *setMemory, unsetMemory: *unsetMemory, probeReminder: *probeReminder,
		writeReminder: *writeReminder, verbose: *verbose,
	}
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type options struct {
	name, peripheralID, helperPath string
	scanTimeout, settle            time.Duration
	doWrites, asOwner, verbose     bool
	hold, listen                   time.Duration
	unsetMemory                    int
	setMemory                      string
	probeReminder, writeReminder   bool
}

// parseSetMemory reads the N=value form of -set-memory.
func parseSetMemory(s string) (position, extension int, err error) {
	lhs, rhs, ok := strings.Cut(s, "=")
	if !ok {
		return 0, 0, fmt.Errorf("want N=value, e.g. 2=4800")
	}
	position, err = strconv.Atoi(strings.TrimSpace(lhs))
	if err != nil || position < 1 || position > 4 {
		return 0, 0, fmt.Errorf("position must be 1-4")
	}
	extension, err = strconv.Atoi(strings.TrimSpace(rhs))
	if err != nil || extension < 0 || extension > 65535 {
		return 0, 0, fmt.Errorf("height must be 0-65535 tenths of a millimetre")
	}
	return position, extension, nil
}

func run(o options) error {
	ctx := context.Background()
	name, peripheralID, helperPath := o.name, o.peripheralID, o.helperPath
	scanTimeout, settle := o.scanTimeout, o.settle
	doWrites, unsetMemory, verbose := o.doWrites, o.unsetMemory, o.verbose

	if helperPath == "" {
		if found := findHelper(); found != "" {
			helperPath = found
			fmt.Printf("using helper at %s\n", helperPath)
		}
	}

	var stderr *os.File
	if verbose {
		stderr = os.Stderr
	}
	client, err := ble.Start(ctx, ble.Options{HelperPath: helperPath, Stderr: stderr})
	if err != nil {
		return fmt.Errorf("starting CoreBluetooth helper: %w\n\n%s", err, helperHint())
	}
	defer client.Close()

	if peripheralID == "" {
		peripheralID, err = findDesk(ctx, client, name, scanTimeout)
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

	device := dpg.NewDevice(client, peripheralID)
	gatt := &dpg.GATT{}
	panel, err := gatt.GetCharacteristic(device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("desk panel characteristic: %w", err)
	}

	rec := &recorder{stimulus: "(before any request)"}
	go rec.collect(client.Notifications(), peripheralID)

	// A DPG controller drops the connection on its own partway through a
	// session. Without watching for it, the only symptom is CoreBluetooth
	// reporting "service not found" - it nils out a disconnected
	// peripheral's services - which looks like an unrelated GATT problem.
	var dropMu sync.Mutex
	dropped := false
	go func() {
		for d := range client.Disconnections() {
			if !strings.EqualFold(d.PeripheralID, peripheralID) {
				continue
			}
			dropMu.Lock()
			dropped = true
			dropMu.Unlock()
			if d.Err != nil {
				rec.note(fmt.Sprintf("desk disconnected: %v", d.Err))
			} else {
				rec.note("desk disconnected (no error reported)")
			}
		}
	}()

	subscribe := func() error {
		nctx, ncancel := context.WithTimeout(ctx, 10*time.Second)
		err := client.SetNotify(nctx, peripheralID, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel, true)
		ncancel()
		if err != nil {
			return fmt.Errorf("desk panel: %w", err)
		}
		return nil
	}

	if err := subscribe(); err != nil {
		return fmt.Errorf("enabling notifications: %w", err)
	}

	// Let anything the controller volunteers on subscribe land before the
	// first request, so it is not blamed on that request.
	time.Sleep(settle)

	// rediscover rebuilds the GATT handles after CoreBluetooth invalidates
	// them. A DPG controller does this partway through a session - it
	// appears to re-present its GATT table - and every write afterwards
	// fails with "service not found" before reaching the desk. Notifications
	// have to be re-armed on the new characteristic too.
	rediscover := func() error {
		dropMu.Lock()
		wasDropped := dropped
		dropped = false
		dropMu.Unlock()

		if wasDropped {
			rec.note("reconnecting")
			cctx, ccancel := context.WithTimeout(ctx, 40*time.Second)
			err := client.Connect(cctx, peripheralID, 30*time.Second)
			ccancel()
			if err != nil {
				return fmt.Errorf("reconnect: %w", err)
			}
			rec.note("reconnected")
		}

		gatt.Disconnect(device)
		fresh, err := gatt.GetCharacteristic(device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
		if err != nil {
			return err
		}
		panel = fresh

		if err := subscribe(); err != nil {
			return err
		}

		return nil
	}

	invalidated := func(err error) bool {
		s := err.Error()
		return strings.Contains(s, "service not found") ||
			strings.Contains(s, "characteristic not found")
	}

	probe := func(label string, payload []byte) {
		rec.begin(label)
		fmt.Printf("\n--> %-28s %s\n", label, hex(payload))

		_, err := panel.Write(payload)
		if err != nil && invalidated(err) {
			fmt.Println("    handles are stale (the desk has most likely dropped); recovering")
			if rerr := rediscover(); rerr != nil {
				fmt.Printf("    rediscovery failed: %v\n", rerr)
				time.Sleep(settle)
				return
			}
			_, err = panel.Write(payload)
		}
		if err != nil {
			fmt.Printf("    write failed: %v\n", err)
		}
		time.Sleep(settle)
	}

	// --- Reads -------------------------------------------------------
	// Each should produce one full response. Any short frame appearing
	// here is a clue in itself.
	for _, r := range []struct {
		label string
		cmd   dpg.DeskPanelCommand
	}{
		{"read capabilities", dpg.DeskPanelCommandGetCapabilities},
		{"read base offset", dpg.DeskPanelCommandBaseOffset},
		{"read product info", dpg.DeskPanelCommandProductInfo},
		{"read user id", dpg.DeskPanelCommandUserID},
		{"read reminder setting", dpg.DeskPanelCommandReminderSetting},
	} {
		probe(r.label, dpg.Envelope(r.cmd))
	}
	readMemoryPositions(probe, "")

	// --- Ownership ----------------------------------------------------
	// A read is not supposed to need the owner bit, but the controller
	// ignores writes without it, so it is worth ruling out.
	if o.asOwner {
		user, ok := rec.userID()
		switch {
		case !ok:
			fmt.Println("\n(skipping -owner: no user id response was seen)")
		case len(user) > 0 && user[0] == 1:
			fmt.Println("\n(already the owner; repeating the memory reads anyway)")
			readMemoryPositions(probe, "as owner")
		default:
			owned := append([]byte(nil), user...)
			owned[0] = 1
			probe("set owner bit", dpg.EnvelopeWithPayload(dpg.DeskPanelCommandUserID, owned))
			readMemoryPositions(probe, "as owner")
		}
	}

	// --- Writes ------------------------------------------------------
	// Every write writes back exactly what was just read, so the desk's
	// stored state is unchanged; only the acknowledgement is of interest.
	if doWrites {
		if base, ok := rec.baseOffset(); ok {
			b := dpg.NewHeight(base).Bytes()
			probe(fmt.Sprintf("write base offset %d", base),
				dpg.EnvelopeWithPayload(dpg.DeskPanelCommandBaseOffset, b[:]))
		} else {
			fmt.Println("\n(skipping the base offset write: no base offset response was seen)")
		}

		if user, ok := rec.userID(); ok {
			probe("write user id (unchanged)",
				dpg.EnvelopeWithPayload(dpg.DeskPanelCommandUserID, user))
		} else {
			fmt.Println("\n(skipping the user id write: no user id response was seen)")
		}
	}

	// --- Set one position outright -------------------------------------
	if o.setMemory != "" {
		n, ext, perr := parseSetMemory(o.setMemory)
		if perr != nil {
			return fmt.Errorf("-set-memory: %w", perr)
		}
		m := dpg.MemoryPosition{Position: n, Extension: &ext}
		probe(fmt.Sprintf("set memory position %d to %d", n, ext),
			m.Envelope())
		verify := fmt.Sprintf("read memory position %d after setting", n)
		probe(verify, dpg.Envelope(m.Command()))

		if got, ok := rec.memoryExtension(verify); ok && got == ext {
			fmt.Printf("\n    memory position %d is now %d\n", n, ext)
		} else {
			fmt.Printf("\n    *** memory position %d did not take the value: wanted %d, got %d ***\n", n, ext, got)
		}
	}

	// --- Destructive ---------------------------------------------------
	// Clearing an *in-range* position is the only way to tell what the
	// short reply means. On a controller reporting two memory positions,
	// asking about position 3 or 4 conflates "set but empty" with "no such
	// position", because both are true at once. Clearing position 2 and
	// asking again separates them: a short reply there means empty, since
	// the position certainly exists.
	//
	// The value is captured first and written back afterwards, so a
	// position in daily use survives the experiment.
	if unsetMemory != 0 {
		n := unsetMemory
		readLabel := fmt.Sprintf("read memory position %d", n)
		original, hadValue := rec.memoryExtension(readLabel)

		fmt.Printf("\n*** memory position %d ***\n", n)
		if hadValue {
			fmt.Printf("    currently %d tenths of a millimetre; it will be restored afterwards\n", original)
		} else {
			fmt.Println("    no height was reported for it, so there is nothing to restore")
		}

		m := dpg.MemoryPosition{Position: n}
		probe(fmt.Sprintf("clear memory position %d", n),
			m.Envelope())
		probe(fmt.Sprintf("read memory position %d after clearing", n),
			dpg.Envelope(m.Command()))

		if hadValue {
			restore := dpg.MemoryPosition{Position: n, Extension: &original}
			probe(fmt.Sprintf("restore memory position %d to %d", n, original),
				restore.Envelope())
			verifyLabel := fmt.Sprintf("read memory position %d after restoring", n)
			probe(verifyLabel, dpg.Envelope(m.Command()))

			if got, ok := rec.memoryExtension(verifyLabel); ok && got == original {
				fmt.Printf("\n    memory position %d restored to %d\n", n, original)
			} else {
				fmt.Printf("\n    *** memory position %d was NOT restored ***\n", n)
				fmt.Printf("    set it back to %d tenths of a millimetre (%.1f mm above the base)\n",
					original, float64(original)/10)
				fmt.Println("    from the desk panel, or with WriteMemoryPosition.")
			}
		}
	}

	// --- Reminder: is the extension byte height-specific? ---------------
	// Base offset and the memory positions take an extension byte before
	// their payload; user ID does not. Both that take it write a height,
	// which is the basis for thinking the byte belongs to heights rather
	// than to writes in general - but nothing has tested a command that
	// writes a non-height value large enough to notice a one-byte shift.
	// Reminder settings are that command, and nothing in dpg writes them,
	// so getting it wrong costs only the settings this restores.
	if o.probeReminder {
		const readLabel = "read reminder setting"
		original, origCounter, ok := rec.reminderSettings(readLabel)

		fmt.Println("\n*** reminder settings ***")
		switch {
		case !ok:
			fmt.Println("    no reminder response was seen, so there is nothing to write back; skipping")
		default:
			fmt.Printf("    currently %s (counter %s); they will be restored afterwards\n",
				hex(original), hex(origCounter))

			// Writing the values just read means a write the desk accepts
			// changes nothing. A write it MISparses is the whole point, so
			// the restore below is not optional. The counter is sent as
			// zeros either way; the desk keeps its own.
			payload := append(append([]byte(nil), original...), make([]byte, len(origCounter))...)

			// The form dpg uses today: NeedsExtensionByte is false for this
			// command, so EnvelopeWithPayload emits no extra byte.
			plain := dpg.EnvelopeWithPayload(dpg.DeskPanelCommandReminderSetting, payload)
			// The same write with an extension byte forced in, built by
			// hand because the package has no way to ask for one here.
			withExt := append([]byte{dpg.EnvelopePrefix, byte(dpg.DeskPanelCommandReminderSetting),
				dpg.EnvelopeDataMarker, 1}, payload...)

			probe("write reminder, no extension byte", plain)
			probe("read reminder after plain write", dpg.Envelope(dpg.DeskPanelCommandReminderSetting))
			afterPlain, plainCounter, gotPlain := rec.reminderSettings("read reminder after plain write")

			probe("write reminder, with extension byte", withExt)
			probe("read reminder after extended write", dpg.Envelope(dpg.DeskPanelCommandReminderSetting))
			afterExt, extCounter, gotExt := rec.reminderSettings("read reminder after extended write")

			// Restore unconditionally, in the form the reads say works.
			probe("restore reminder settings", plain)
			probe("read reminder after restoring", dpg.Envelope(dpg.DeskPanelCommandReminderSetting))
			restored, restoredCounter, gotRestored := rec.reminderSettings("read reminder after restoring")

			fmt.Println("\n    --- settings, which a write controls ---")
			fmt.Printf("    before                  %s\n", hex(original))
			fmt.Printf("    after plain write       %s\n", settingsOrNothing(afterPlain, gotPlain))
			fmt.Printf("    after extended write    %s\n", settingsOrNothing(afterExt, gotExt))
			fmt.Printf("    after restoring         %s\n", settingsOrNothing(restored, gotRestored))

			fmt.Println("\n    --- counter, which the desk maintains ---")
			fmt.Printf("    before                  %-14s %s\n", hex(origCounter), counterNote(origCounter))
			fmt.Printf("    after plain write       %-14s %s\n", hex(plainCounter), counterNote(plainCounter))
			fmt.Printf("    after extended write    %-14s %s\n", hex(extCounter), counterNote(extCounter))
			fmt.Printf("    after restoring         %-14s %s\n", hex(restoredCounter), counterNote(restoredCounter))

			plainKept := gotPlain && bytes.Equal(afterPlain, original)
			extKept := gotExt && bytes.Equal(afterExt, original)
			switch {
			case !plainKept:
				fmt.Println("\n    INCONCLUSIVE: writing the settings back unchanged, in the form")
				fmt.Println("    dpg uses today, did not round-trip. Either reminder settings")
				fmt.Println("    cannot be written this way at all or the write was ignored, so")
				fmt.Println("    this run says nothing about the extension byte.")
			case extKept:
				fmt.Println("\n    Both forms round-tripped, so the desk ignores an extension")
				fmt.Println("    byte on this command. That makes the byte a property of which")
				fmt.Println("    commands parse it, not of what follows it.")
			default:
				fmt.Println("\n    Only the form WITHOUT an extension byte round-tripped, so the")
				fmt.Println("    extra byte shifted the payload. The extension byte belongs to")
				fmt.Println("    the height-carrying commands, as NeedsExtensionByte has it.")
			}

			if !gotRestored || !bytes.Equal(restored, original) {
				fmt.Printf("\n    *** reminder settings were NOT restored: wanted %s ***\n", hex(original))
				fmt.Println("    set them again from the desk panel.")
			} else {
				fmt.Println("\n    Reminder settings are back as they were; the counter moving on")
				fmt.Println("    is the desk recording the write, not a leftover change.")
			}
		}
	}

	// --- WriteReminder: does the desk accept what dpg packs? ------------
	// Everything else here writes bytes this file builds. This one goes
	// through desk.WriteReminder, because that is the code under test: the
	// read direction is confirmed, and Reminder.Bytes packs to match it,
	// but the two agreeing with each other is exactly the state they were
	// in when both had the pair order backwards.
	//
	// It sets preset 1 to a deliberately lopsided value, so neither minute
	// can be mistaken for the other, and restores afterwards by the raw
	// path that is known to work.
	if o.writeReminder {
		const (
			readLabel    = "read reminder setting"
			wantSitting  = 23
			wantStanding = 61
		)
		original, _, ok := rec.reminderSettings(readLabel)

		fmt.Println("\n*** desk.WriteReminder ***")
		switch {
		case !ok:
			fmt.Println("    no reminder response was seen, so there is nothing to restore to; skipping")
		case len(original) < reminderSettingsLen:
			fmt.Printf("    the reminder response was only %d bytes; skipping\n", len(original))
		default:
			fmt.Printf("    currently %s; it will be restored afterwards\n", hex(original))

			// Writes are ignored outright without the owner bit, and a
			// silently ignored write looks exactly like a rejected one.
			if user, uok := rec.userID(); !uok {
				fmt.Println("    no user id response was seen, so ownership cannot be asserted; skipping")
				break
			} else if len(user) > 0 && user[0] != 1 {
				owned := append([]byte(nil), user...)
				owned[0] = 1
				probe("set owner bit", dpg.EnvelopeWithPayload(dpg.DeskPanelCommandUserID, owned))
			}

			// Decoded here rather than with deskpanel's parser, so this
			// check does not lean on the code it is checking.
			want := dpg.Reminder{
				ActiveOption: dpg.ReminderOption(original[0]),
				Option1:      dpg.ReminderIntervals{MinutesSitting: wantSitting, MinutesStanding: wantStanding},
				Option2:      dpg.ReminderIntervals{MinutesSitting: int(original[3]), MinutesStanding: int(original[4])},
				Option3:      dpg.ReminderIntervals{MinutesSitting: int(original[5]), MinutesStanding: int(original[6])},
			}

			rec.begin("desk.WriteReminder preset 1")
			fmt.Printf("\n--> %-28s %+v\n", "desk.WriteReminder", want.Option1)
			d := deskpkg.New(device, "frames")
			err := d.WriteReminder(want)
			if err != nil && invalidated(err) {
				fmt.Println("    handles are stale (the desk has most likely dropped); recovering")
				if rerr := rediscover(); rerr != nil {
					fmt.Printf("    rediscovery failed: %v\n", rerr)
				} else {
					d = deskpkg.New(device, "frames")
					err = d.WriteReminder(want)
				}
			}
			if err != nil {
				fmt.Printf("    WriteReminder returned: %v\n", err)
			}
			time.Sleep(settle)

			probe("read reminder after WriteReminder", dpg.Envelope(dpg.DeskPanelCommandReminderSetting))
			after, _, gotAfter := rec.reminderSettings("read reminder after WriteReminder")

			// Restore by the raw path, which -probe-reminder showed works.
			restorePayload := append(append([]byte(nil), original...), 0, 0, 0, 0)
			probe("restore reminder settings", dpg.EnvelopeWithPayload(dpg.DeskPanelCommandReminderSetting, restorePayload))
			probe("read reminder after restoring", dpg.Envelope(dpg.DeskPanelCommandReminderSetting))
			restored, _, gotRestored := rec.reminderSettings("read reminder after restoring")

			fmt.Println("\n    --- what desk.WriteReminder stored ---")
			fmt.Printf("    before                  %s\n", hex(original))
			fmt.Printf("    asked for               preset 1 = %d sitting / %d standing\n", wantSitting, wantStanding)
			fmt.Printf("    after WriteReminder     %s\n", settingsOrNothing(after, gotAfter))
			fmt.Printf("    after restoring         %s\n", settingsOrNothing(restored, gotRestored))

			switch {
			case !gotAfter:
				fmt.Println("\n    INCONCLUSIVE: the desk did not answer the read after the write.")
			case bytes.Equal(after, original):
				fmt.Println("\n    The settings did not change at all, so the write never took")
				fmt.Println("    effect. That is ownership or an unwritable command, not the")
				fmt.Println("    packing - this run says nothing about the pair order.")
			case after[1] == wantSitting && after[2] == wantStanding:
				fmt.Println("\n    desk.WriteReminder works: the desk stored the sitting minutes")
				fmt.Println("    first, exactly as Reminder.Bytes packs them. The write path")
				fmt.Println("    now matches the read path against real hardware.")
			case after[1] == wantStanding && after[2] == wantSitting:
				fmt.Println("\n    *** Reminder.Bytes has the pair backwards: the desk stored")
				fmt.Println("    standing first. Swap the two in Reminder.Bytes, and swap them")
				fmt.Println("    in TestReminderPacksAndParsesSymmetrically with it. ***")
			default:
				fmt.Println("\n    The settings changed, but into something neither expected")
				fmt.Println("    ordering predicts. The frame log above is the place to start.")
			}

			if !gotRestored || !bytes.Equal(restored, original) {
				fmt.Printf("\n    *** reminder settings were NOT restored: wanted %s ***\n", hex(original))
				fmt.Println("    set them again from the desk panel.")
			} else {
				fmt.Println("\n    Reminder settings are back as they were.")
			}
		}
	}

	// --- Hold ----------------------------------------------------------
	// --- Unprompted frames --------------------------------------------
	// Everything else here is request/response. This asks the opposite
	// question: does the desk ever speak first? Storing a favourite is the
	// only thing the panel can change, so it is the case to try.
	if o.listen > 0 {
		listenForUnprompted(rec, o, &dropMu, &dropped)
	}

	// The requests above take about twenty seconds; a bridge holds a
	// connection for hours. This measures how long the link survives with
	// nothing happening on it.
	if o.hold > 0 {
		holdConnection(rec, o, &dropMu, &dropped)
	}

	rec.report(os.Stdout)
	return nil
}

// listenForUnprompted sits quiet with notifications still enabled and
// reports anything that arrives. Nothing is requested during it, so every
// frame it records was the desk's own idea.
//
// The answer shapes the API: a request/response wrapper around the
// DeskPanel characteristic can only be the whole story if the desk never
// speaks first. If storing a favourite does push a frame, it arrives under
// the same response code as the answer to a memory read and says no more
// about which position it describes - so a pending read could take it for
// its own reply.
func listenForUnprompted(rec *recorder, o options, dropMu *sync.Mutex, dropped *bool) {
	const label = "unprompted (nothing was requested)"
	rec.begin(label)

	fmt.Printf("\n--> listening for %v with nothing outstanding\n", o.listen)
	fmt.Println("    Use the panel now: store a favourite, move the desk, change the")
	fmt.Println("    reminder setting. Anything printed below arrived unasked.")

	dropMu.Lock()
	*dropped = false
	dropMu.Unlock()

	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()

	start := time.Now()
	deadline := time.After(o.listen)
	lastReport := time.Now()

listening:
	for {
		select {
		case <-deadline:
			break listening

		case <-poll.C:
			dropMu.Lock()
			gone := *dropped
			dropMu.Unlock()
			if gone {
				fmt.Printf("\n    the desk dropped after %v of listening\n", time.Since(start).Round(time.Second))
				break listening
			}
			if time.Since(lastReport) >= 30*time.Second {
				lastReport = time.Now()
				fmt.Printf("    still listening, %v elapsed\n", time.Since(start).Round(time.Second))
			}
		}
	}

	var panelFrames []frame
	for _, f := range rec.framesFor(label) {
		if f.note == "" && f.char == "panel" {
			panelFrames = append(panelFrames, f)
		}
	}

	fmt.Println("\n    --- unprompted DeskPanel frames ---")
	if len(panelFrames) == 0 {
		fmt.Println("    none. As far as this run shows, the desk only ever answers, so a")
		fmt.Println("    request/response API over DeskPanel loses nothing. Worth")
		fmt.Println("    repeating before relying on it - a quiet run only means nothing")
		fmt.Println("    happened to provoke one.")
		return
	}
	for _, f := range panelFrames {
		fmt.Printf("    %-8s %2d bytes  %-36s %s\n",
			f.since.Round(time.Millisecond), len(f.data), hex(f.data), dec(f.data))
	}
	fmt.Printf("\n    %d unprompted frame(s). The desk speaks first, so DeskPanel is not\n", len(panelFrames))
	fmt.Println("    purely request/response: a reader has to stay available for these,")
	fmt.Println("    and a pending query has to be able to tell one from its own reply.")
}

func holdConnection(rec *recorder, o options, dropMu *sync.Mutex, dropped *bool) {
	rec.begin("holding the connection open")
	fmt.Printf("\n--> holding for %v, idle\n", o.hold)

	// Anything reported for the previous phase is not this one's business.
	dropMu.Lock()
	*dropped = false
	dropMu.Unlock()

	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()

	start := time.Now()
	deadline := time.After(o.hold)
	lastReport := time.Now()

	for {
		select {
		case <-deadline:
			fmt.Printf("\n    held for %v with no disconnect\n", o.hold)
			return

		case <-poll.C:
			dropMu.Lock()
			gone := *dropped
			dropMu.Unlock()
			if gone {
				fmt.Printf("\n    dropped after %v of holding\n", time.Since(start).Round(time.Second))
				return
			}
			if time.Since(lastReport) >= 30*time.Second {
				lastReport = time.Now()
				fmt.Printf("    still connected after %v\n", time.Since(start).Round(time.Second))
			}
		}
	}
}

// readMemoryPositions asks for all four memory positions in turn.
func readMemoryPositions(probe func(string, []byte), suffix string) {
	if suffix != "" {
		suffix = " (" + suffix + ")"
	}
	for i, cmd := range []dpg.DeskPanelCommand{
		dpg.DeskPanelCommandMemoryPosition1,
		dpg.DeskPanelCommandMemoryPosition2,
		dpg.DeskPanelCommandMemoryPosition3,
		dpg.DeskPanelCommandMemoryPosition4,
	} {
		probe(fmt.Sprintf("read memory position %d%s", i+1, suffix), dpg.Envelope(cmd))
	}
}

// helperCandidates are where corebluetoothd ends up after a `make helper`
// or `make example` in the sibling checkouts, relative to the directory
// this is run from (the dpg repo root, for `go run ./example/frames`).
//
// ble.Start's own lookup is no help here. It checks next to the running
// executable, which under `go run` is a temporary build directory, and
// then $PATH — where an entry has to name the directory holding the
// executable itself, .../corebluetoothd.app/Contents/MacOS, rather than
// the .app bundle or the directory containing it.
var helperCandidates = []string{
	"bin/corebluetoothd.app/Contents/MacOS/corebluetoothd",
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
	b.WriteString("The corebluetoothd helper was not found. Build it with:\n\n")
	b.WriteString("    make -C ../corebluetooth-go helper\n\n")
	b.WriteString("and it will be picked up automatically. Looked in:\n")
	for _, c := range helperCandidates {
		b.WriteString("    " + c + "\n")
	}
	b.WriteString("\nOtherwise pass -helper with the path to the executable *inside* the\n")
	b.WriteString("bundle, not the bundle itself:\n\n")
	b.WriteString("    -helper .../corebluetoothd.app/Contents/MacOS/corebluetoothd\n\n")
	b.WriteString("Note that the bundle is what macOS reads the Bluetooth authorization\n")
	b.WriteString("identity from, so a bare binary built outside it will be killed on its\n")
	b.WriteString("first CoreBluetooth call rather than merely denied.")
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

// ---------------------------------------------------------------------
// Recording
// ---------------------------------------------------------------------

type frame struct {
	at       time.Time
	since    time.Duration // since its stimulus was sent
	stimulus string
	data     []byte
	char     string // which characteristic delivered it
	note     string // set instead of data for connection events
}

type recorder struct {
	mu       sync.Mutex
	stimulus string
	stimAt   time.Time
	frames   []frame
	order    []string // stimulus labels, in the order they were issued
}

// begin marks the start of a new stimulus. Frames are attributed to the
// most recent one, so a late reply lands on the request that caused it
// rather than on the next request.
func (r *recorder) begin(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stimulus = label
	r.stimAt = time.Now()
	r.order = append(r.order, label)
}

func (r *recorder) collect(updates <-chan ble.CharacteristicUpdate, peripheralID string) {
	for u := range updates {
		if !strings.EqualFold(u.PeripheralID, peripheralID) {
			continue
		}
		var tag string
		switch {
		case strings.EqualFold(u.CharacteristicUUID, dpg.CharacteristicUUIDDeskPanel):
			tag = "panel"
		case strings.EqualFold(u.CharacteristicUUID, dpg.CharacteristicUUIDReferenceOutput):
			tag = "refout"
		default:
			continue
		}
		data, err := base64.StdEncoding.DecodeString(u.ValueBase64)
		if err != nil {
			continue
		}

		r.mu.Lock()
		f := frame{at: time.Now(), stimulus: r.stimulus, data: data, char: tag}
		if !r.stimAt.IsZero() {
			f.since = f.at.Sub(r.stimAt)
		}
		r.frames = append(r.frames, f)
		r.mu.Unlock()

		fmt.Printf("    <-- %-6s %2d bytes  %-36s %s\n", tag, len(data), hex(data), dec(data))
	}
}

// note records something that is not a frame - a disconnect, a
// reconnect - on the same timeline, so the report shows where in the
// sequence it happened.
func (r *recorder) note(text string) {
	r.mu.Lock()
	f := frame{at: time.Now(), stimulus: r.stimulus, note: text}
	if !r.stimAt.IsZero() {
		f.since = f.at.Sub(r.stimAt)
	}
	r.frames = append(r.frames, f)
	r.mu.Unlock()
	fmt.Printf("    ### %s\n", text)
}

func (r *recorder) snapshot() []frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]frame(nil), r.frames...)
}

// baseOffset returns the base offset from whichever response carried one,
// so it can be written straight back unchanged.
func (r *recorder) baseOffset() (int, bool) {
	for _, f := range r.snapshot() {
		if isResponse(f.data, dpg.DeskPanelResponseBaseOffset) && len(f.data) >= 5 {
			return int(binary.LittleEndian.Uint16(f.data[3:5])), true
		}
	}
	return 0, false
}

// userID returns the stored user ID verbatim, owner bit and all.
func (r *recorder) userID() ([]byte, bool) {
	for _, f := range r.snapshot() {
		if isResponse(f.data, dpg.DeskPanelResponseUserID) && len(f.data) > 2 {
			return append([]byte(nil), f.data[2:]...), true
		}
	}
	return nil, false
}

// memoryExtension returns the extension reported in answer to one
// specific request. Requests are issued one at a time with a settle
// between them, so the code-7 frame recorded against that label is that
// position's - the frame itself says nothing about which position it
// describes.
func (r *recorder) memoryExtension(stimulus string) (int, bool) {
	for _, f := range r.snapshot() {
		if f.stimulus != stimulus || f.note != "" {
			continue
		}
		if isResponse(f.data, dpg.DeskPanelResponseMemoryPosition) && len(f.data) >= 5 {
			return int(binary.LittleEndian.Uint16(f.data[3:5])), true
		}
	}
	return 0, false
}

// A reminder response carries eleven bytes after its two-byte header:
// seven settings, then a counter the desk maintains for itself. Reminder
// .Bytes() sends those four as zeros and the desk substitutes its own,
// exactly as a memory position does, so they are not part of what a write
// round-trips and comparing them would fail every time.
const reminderSettingsLen = 7

// reminderSettings returns the settings bytes of the reminder response
// recorded against one request, and the counter separately. Two reads can
// then be compared on the part a write actually controls.
func (r *recorder) reminderSettings(stimulus string) (settings, counter []byte, ok bool) {
	for _, f := range r.snapshot() {
		if f.stimulus != stimulus || f.note != "" {
			continue
		}
		if !isResponse(f.data, dpg.DeskPanelResponseReminderSetting) || len(f.data) <= 2 {
			continue
		}
		payload := append([]byte(nil), f.data[2:]...)
		if len(payload) <= reminderSettingsLen {
			return payload, nil, true
		}
		return payload[:reminderSettingsLen], payload[reminderSettingsLen:], true
	}
	return nil, nil, false
}

// framesFor returns the frames recorded against one stimulus, notes and
// all, in the order they arrived.
func (r *recorder) framesFor(stimulus string) []frame {
	var out []frame
	for _, f := range r.snapshot() {
		if f.stimulus == stimulus {
			out = append(out, f)
		}
	}
	return out
}

func isResponse(data []byte, code int) bool {
	return len(data) >= 2 && data[0] == 1 && data[1] == byte(code)
}

// report prints what was seen, per stimulus, and then the two summaries
// the whole exercise is for.
func (r *recorder) report(w *os.File) {
	frames := r.snapshot()

	fmt.Fprintln(w, "\n\n=========== frames by request ===========")
	byStimulus := map[string][]frame{}
	for _, f := range frames {
		byStimulus[f.stimulus] = append(byStimulus[f.stimulus], f)
	}

	r.mu.Lock()
	order := append([]string{"(before any request)"}, r.order...)
	r.mu.Unlock()

	seen := map[string]bool{}
	for _, label := range order {
		if seen[label] {
			continue
		}
		seen[label] = true

		fs := byStimulus[label]
		if len(fs) == 0 {
			fmt.Fprintf(w, "\n%s\n    (no frames)\n", label)
			continue
		}
		fmt.Fprintf(w, "\n%s\n", label)
		for _, f := range fs {
			if f.note != "" {
				fmt.Fprintf(w, "    +%-8s ### %s\n", f.since.Round(time.Millisecond), f.note)
				continue
			}
			fmt.Fprintf(w, "    +%-8s %-6s %2d bytes  %-36s %s\n",
				f.since.Round(time.Millisecond), f.char, len(f.data), hex(f.data), dec(f.data))
		}
	}

	// The capabilities response says how many memory positions the
	// controller claims to have. If that is zero, a silent memory read is
	// the controller saying it has none rather than anything being wrong.
	for _, f := range frames {
		if !isResponse(f.data, dpg.DeskPanelResponseCapabilities) || len(f.data) < 3 {
			continue
		}
		b := f.data[2]
		fmt.Fprintf(w, "\n\n=========== capabilities ===========\n")
		fmt.Fprintf(w, "memory positions: %d\n", b&dpg.CapabilitiesFlagMemSize)
		fmt.Fprintf(w, "auto up:          %t\n", b&dpg.CapabilitiesFlagAutoUp != 0)
		fmt.Fprintf(w, "auto down:        %t\n", b&dpg.CapabilitiesFlagAutoDown != 0)
		fmt.Fprintf(w, "ble allowed:      %t\n", b&dpg.CapabilitiesFlagBleAllow != 0)
		fmt.Fprintf(w, "display:          %t\n", b&dpg.CapabilitiesFlagDisplay != 0)
		fmt.Fprintf(w, "light:            %t\n", b&dpg.CapabilitiesFlagLight != 0)
		if b&dpg.CapabilitiesFlagMemSize == 0 {
			fmt.Fprintln(w, "\nThe controller reports no memory positions, which would explain a")
			fmt.Fprintln(w, "silent response to every memory position request.")
		}
		break
	}

	// Where the connection dropped matters more than anything else in the
	// log: everything attempted after a drop and before a recovery never
	// reached the desk.
	fmt.Fprintln(w, "\n\n=========== connection events ===========")
	events := 0
	for _, f := range frames {
		if f.note == "" {
			continue
		}
		events++
		fmt.Fprintf(w, "during %-40s %s\n", f.stimulus, f.note)
	}
	if events == 0 {
		fmt.Fprintln(w, "none — the connection held for the whole run")
	}

	// Short frames are the acknowledgements. If their bytes differ by
	// command, a write can be attributed; if they are all identical, it
	// cannot, and the queue-and-wait approach is the only option.
	fmt.Fprintln(w, "\n\n=========== short frames (acknowledgements) ===========")
	acks := map[string][]string{} // hex -> stimuli that produced it
	for _, f := range frames {
		if f.note != "" || f.char != "panel" {
			continue
		}
		if len(f.data) <= 3 {
			acks[hex(f.data)] = append(acks[hex(f.data)], f.stimulus)
		}
	}
	if len(acks) == 0 {
		fmt.Fprintln(w, "none seen — run again with -writes, since reads may not be acknowledged")
	} else {
		keys := make([]string, 0, len(acks))
		for k := range acks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "\n%s\n", k)
			for _, s := range acks[k] {
				fmt.Fprintf(w, "    after %s\n", s)
			}
		}
		switch {
		case len(acks) == 1:
			fmt.Fprintln(w, "\nOne distinct acknowledgement for every write: nothing in the frame")
			fmt.Fprintln(w, "identifies which write it answers. Attributing them means serialising")
			fmt.Fprintln(w, "writes and pairing each ack with the outstanding one.")
		default:
			fmt.Fprintln(w, "\nMore than one distinct acknowledgement. Check whether the differing")
			fmt.Fprintln(w, "byte tracks the command rather than, say, a status or a counter — if")
			fmt.Fprintln(w, "it tracks the command, writes can be attributed without serialising.")
		}
	}

	// The shortest full response per type is the highest a typed parser's
	// length guard could safely be raised to.
	fmt.Fprintln(w, "\n\n=========== response lengths by type ===========")
	minLen := map[byte]int{}
	maxLen := map[byte]int{}
	for _, f := range frames {
		if f.char != "panel" || len(f.data) < 2 || f.data[0] != 1 || len(f.data) <= 3 {
			continue
		}
		t := f.data[1]
		if n, ok := minLen[t]; !ok || len(f.data) < n {
			minLen[t] = len(f.data)
		}
		if n, ok := maxLen[t]; !ok || len(f.data) > n {
			maxLen[t] = len(f.data)
		}
	}
	if len(minLen) == 0 {
		fmt.Fprintln(w, "none seen")
		return
	}
	types := make([]int, 0, len(minLen))
	for t := range minLen {
		types = append(types, int(t))
	}
	sort.Ints(types)
	fmt.Fprintf(w, "%-24s %-6s %s\n", "RESPONSE", "MIN", "MAX")
	for _, t := range types {
		fmt.Fprintf(w, "%-24s %-6d %d\n", responseName(byte(t)), minLen[byte(t)], maxLen[byte(t)])
	}
	fmt.Fprintln(w, "\nA typed parser's length guard can be raised to that type's MIN.")
}

func responseName(t byte) string {
	switch int(t) {
	case dpg.DeskPanelResponseCapabilities:
		return fmt.Sprintf("capabilities (%d)", t)
	case dpg.DeskPanelResponseBaseOffset:
		return fmt.Sprintf("base offset (%d)", t)
	case dpg.DeskPanelResponseProductInfo:
		return fmt.Sprintf("product info (%d)", t)
	case dpg.DeskPanelResponseMemoryPosition:
		return fmt.Sprintf("memory position (%d)", t)
	case dpg.DeskPanelResponseReminderSetting:
		return fmt.Sprintf("reminder (%d)", t)
	case dpg.DeskPanelResponseUserID:
		return fmt.Sprintf("user id (%d)", t)
	default:
		return fmt.Sprintf("unknown (%d)", t)
	}
}

func hex(b []byte) string {
	if len(b) == 0 {
		return "(empty)"
	}
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, " ")
}

func dec(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// settingsOrNothing renders a read-back settings field, or says none
// arrived - distinguishing "the desk stored something wrong" from "the
// desk said nothing", which mean different things here.
func settingsOrNothing(b []byte, ok bool) string {
	if !ok {
		return "(no response)"
	}
	return hex(b)
}

// counterNote decodes the counter so successive reads can be compared at a
// glance. It behaves like a ~1 Hz timestamp of the last write: it jumps to
// "now" when the desk accepts one and then advances with the clock.
func counterNote(b []byte) string {
	if len(b) != 4 {
		return ""
	}
	return fmt.Sprintf("(%d)", binary.LittleEndian.Uint32(b))
}
