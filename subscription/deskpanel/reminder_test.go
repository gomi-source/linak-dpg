package deskpanel

import (
	"testing"

	"github.com/gomi-source/linak-dpg"
)

// Frames a DPG1M returned for `read reminder setting`, captured by
// example/frames -probe-reminder while switching the selection on the
// panel. The three presets were at their defaults, which is what makes
// these usable fixtures: the values are known independently of how this
// code reads them.
//
// The pairs are identical across all four, which is the point - only the
// first byte moves, so a parser that folded the selection into the
// intervals would fail here.
func TestParseReminder(t *testing.T) {
	// The shipped defaults. See TestParseReminderAsymmetricPreset for the
	// frame that establishes which of each pair is which.
	presets := dpg.Reminder{
		Option1: dpg.ReminderIntervals{MinutesSitting: 55, MinutesStanding: 5},
		Option2: dpg.ReminderIntervals{MinutesSitting: 50, MinutesStanding: 10},
		Option3: dpg.ReminderIntervals{MinutesSitting: 45, MinutesStanding: 15},
	}

	for _, tc := range []struct {
		name   string
		active byte
		want   dpg.ReminderOption
	}{
		{"reminders off", 56, dpg.ReminderOff},
		{"preset 1 active", 57, dpg.ReminderOption1},
		{"preset 2 active", 58, dpg.ReminderOption2},
		{"preset 3 active", 59, dpg.ReminderOption3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := []byte{1, 11, tc.active, 55, 5, 50, 10, 45, 15, 0x9E, 0x22, 0xEF, 0x08}

			got, ok := parseReminder(frame)
			if !ok {
				t.Fatal("parseReminder rejected a full response")
			}

			want := presets
			want.ActiveOption = tc.want
			if got != want {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

// The three pairs must not overlap: reading Option2's first byte from
// Option1's second was the original bug, and it is invisible whenever a
// desk happens to hold symmetric values.
func TestParseReminderPairsDoNotOverlap(t *testing.T) {
	frame := []byte{1, 11, 57, 1, 2, 3, 4, 5, 6, 0, 0, 0, 0}

	got, ok := parseReminder(frame)
	if !ok {
		t.Fatal("parseReminder rejected a full response")
	}
	want := dpg.Reminder{
		ActiveOption: dpg.ReminderOption(57),
		Option1:      dpg.ReminderIntervals{MinutesSitting: 1, MinutesStanding: 2},
		Option2:      dpg.ReminderIntervals{MinutesSitting: 3, MinutesStanding: 4},
		Option3:      dpg.ReminderIntervals{MinutesSitting: 5, MinutesStanding: 6},
	}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// The guard has to cover data[8], the last byte the parser reads.
func TestParseReminderRejectsShortFrames(t *testing.T) {
	if _, ok := parseReminder([]byte{1, 11, 56, 55, 5, 50, 10, 45}); ok {
		t.Error("parseReminder accepted a frame one byte too short to hold Option3")
	}
}

// Reminder.Bytes and parseReminder have to agree on the order within each
// pair. They live in different packages and were written apart, which is
// exactly how they came to disagree: reads put sitting minutes first,
// writes put standing minutes first, so a value read and written back
// would have swapped every preset.
func TestReminderPacksAndParsesSymmetrically(t *testing.T) {
	want := dpg.Reminder{
		ActiveOption: dpg.ReminderOption2,
		Option1:      dpg.ReminderIntervals{MinutesSitting: 55, MinutesStanding: 5},
		Option2:      dpg.ReminderIntervals{MinutesSitting: 50, MinutesStanding: 10},
		Option3:      dpg.ReminderIntervals{MinutesSitting: 45, MinutesStanding: 15},
	}

	packed := want.Bytes()
	// A response carries the same payload behind a two-byte header.
	frame := append([]byte{1, byte(dpg.DeskPanelResponseReminderSetting)}, packed[:]...)

	got, ok := parseReminder(frame)
	if !ok {
		t.Fatal("parseReminder rejected what Reminder.Bytes produced")
	}
	if got != want {
		t.Errorf("round trip changed the settings\ngot  %+v\nwant %+v", got, want)
	}
}

// The frame that settles the order within a pair. Preset 1 was set from
// LINAK's app to stand 60 / sit 20 - deliberately lopsided, and summing to
// 80 rather than the 60 the defaults happen to - so neither byte can be
// mistaken for the other. The desk reported 20 first.
//
// It also pins that editing one preset leaves the others untouched: 2 and
// 3 still hold their defaults.
func TestParseReminderAsymmetricPreset(t *testing.T) {
	frame := []byte{1, 11, 57, 20, 60, 50, 10, 45, 15, 0x9E, 0x22, 0xEF, 0x08}

	got, ok := parseReminder(frame)
	if !ok {
		t.Fatal("parseReminder rejected a full response")
	}

	want := dpg.Reminder{
		ActiveOption: dpg.ReminderOption1,
		Option1:      dpg.ReminderIntervals{MinutesSitting: 20, MinutesStanding: 60},
		Option2:      dpg.ReminderIntervals{MinutesSitting: 50, MinutesStanding: 10},
		Option3:      dpg.ReminderIntervals{MinutesSitting: 45, MinutesStanding: 15},
	}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}
