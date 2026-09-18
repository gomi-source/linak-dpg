package dpg

import (
	"bytes"
	"testing"
)

// The envelope WriteReminder puts on the wire. Command 136 takes no
// extension byte - proven by example/frames -probe-reminder, where forcing
// one in shifted the stored settings by a byte - so the payload follows
// the data marker directly.
func TestReminderEnvelope(t *testing.T) {
	r := Reminder{
		ActiveOption: ReminderOption1,
		Option1:      ReminderIntervals{MinutesSitting: 20, MinutesStanding: 60},
		Option2:      ReminderIntervals{MinutesSitting: 50, MinutesStanding: 10},
		Option3:      ReminderIntervals{MinutesSitting: 45, MinutesStanding: 15},
	}

	packed := r.Bytes()
	got := EnvelopeWithPayload(DeskPanelCommandReminderSetting, packed[:])
	want := []byte{
		EnvelopePrefix, 136, EnvelopeDataMarker,
		57,     // preset 1 active
		20, 60, // preset 1: sitting, standing
		50, 10, // preset 2
		45, 15, // preset 3
		0, 0, 0, 0, // counter; the desk substitutes its own
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestReminderIsValid(t *testing.T) {
	ok := Reminder{
		ActiveOption: ReminderOff,
		Option1:      ReminderIntervals{MinutesSitting: 55, MinutesStanding: 5},
		Option2:      ReminderIntervals{MinutesSitting: 50, MinutesStanding: 10},
		Option3:      ReminderIntervals{MinutesSitting: 45, MinutesStanding: 15},
	}
	if !ok.IsValid() {
		t.Error("rejected settings a desk reported verbatim")
	}

	for _, tc := range []struct {
		name string
		mut  func(*Reminder)
	}{
		{"unknown active option", func(r *Reminder) { r.ActiveOption = ReminderOption(60) }},
		// 300 would pack as 44 and look deliberate.
		{"sitting minutes past a byte", func(r *Reminder) { r.Option2.MinutesSitting = 300 }},
		{"standing minutes past a byte", func(r *Reminder) { r.Option3.MinutesStanding = 256 }},
		{"negative interval", func(r *Reminder) { r.Option1.MinutesStanding = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := ok
			tc.mut(&bad)
			if bad.IsValid() {
				t.Errorf("accepted %+v", bad)
			}
		})
	}
}
