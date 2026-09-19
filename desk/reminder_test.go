package desk

import (
	"context"
	"testing"

	"github.com/gomi-source/linak-dpg"
)

// The point of these wrappers: a caller changing one preset must not have
// to know that the controller rewrites all of them.
func TestWriteReminderPresetChangesOnlyThatPreset(t *testing.T) {
	current := dpg.Reminder{
		ActiveOption: dpg.ReminderOption2,
		Option1:      dpg.ReminderIntervals{MinutesSitting: 55, MinutesStanding: 5},
		Option2:      dpg.ReminderIntervals{MinutesSitting: 50, MinutesStanding: 10},
		Option3:      dpg.ReminderIntervals{MinutesSitting: 45, MinutesStanding: 15},
	}
	want := dpg.ReminderIntervals{MinutesSitting: 20, MinutesStanding: 60}

	for _, tc := range []struct {
		preset int
		pick   func(dpg.Reminder) dpg.ReminderIntervals
	}{
		{1, func(r dpg.Reminder) dpg.ReminderIntervals { return r.Option1 }},
		{2, func(r dpg.Reminder) dpg.ReminderIntervals { return r.Option2 }},
		{3, func(r dpg.Reminder) dpg.ReminderIntervals { return r.Option3 }},
	} {
		got := current
		applyPreset(&got, tc.preset, want)

		if tc.pick(got) != want {
			t.Errorf("preset %d = %+v, want %+v", tc.preset, tc.pick(got), want)
		}
		if got.ActiveOption != current.ActiveOption {
			t.Errorf("preset %d changed the active option", tc.preset)
		}
		// The other two must be untouched.
		for _, other := range []struct {
			n    int
			pick func(dpg.Reminder) dpg.ReminderIntervals
		}{
			{1, func(r dpg.Reminder) dpg.ReminderIntervals { return r.Option1 }},
			{2, func(r dpg.Reminder) dpg.ReminderIntervals { return r.Option2 }},
			{3, func(r dpg.Reminder) dpg.ReminderIntervals { return r.Option3 }},
		} {
			if other.n == tc.preset {
				continue
			}
			if other.pick(got) != other.pick(current) {
				t.Errorf("writing preset %d also changed preset %d", tc.preset, other.n)
			}
		}
	}
}

// Out-of-range presets and unknown options are rejected before anything
// reaches the desk - a bad active option would put the controller in a
// state nothing here has explored.
func TestReminderWrappersRejectBadInput(t *testing.T) {
	d := &Desk{}
	ctx := context.Background()

	for _, preset := range []int{0, 4, -1} {
		if err := d.WriteReminderPreset(ctx, preset, dpg.ReminderIntervals{}); err == nil {
			t.Errorf("preset %d was accepted", preset)
		}
	}
	for _, option := range []dpg.ReminderOption{0, 55, 60, 200} {
		if err := d.WriteActiveReminder(ctx, option); err == nil {
			t.Errorf("active option %d was accepted", option)
		}
	}
}
