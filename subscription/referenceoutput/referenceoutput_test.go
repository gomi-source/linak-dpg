package referenceoutput

import "testing"

// The frames here are from a DPG1M run into an obstruction by
// example/moves -scenarios blocked.
func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		frame []byte
		want  Reading
	}{
		{"up, full speed", []byte{0x12, 0x05, 0x50, 0x18}, Reading{Extension: 1298, Speed: 6224}},
		{"down, full speed", []byte{0x12, 0x05, 0xB0, 0xE7}, Reading{Extension: 1298, Speed: -6224}},
		{"at rest", []byte{0xDB, 0x05, 0x00, 0x00}, Reading{Extension: 1499}},
		{"stalled against the obstruction", []byte{0x9F, 0x04, 0x02, 0x00}, Reading{Extension: 1183, Flags: FlagCollisionRecovery}},
		{"backing away", []byte{0x59, 0x05, 0x52, 0x18}, Reading{Extension: 1369, Speed: 6224, Flags: FlagCollisionRecovery}},
		// Not seen, but it is what the bits would mean: a flagged report
		// going down must stay negative.
		{"flagged going down", []byte{0x59, 0x05, 0xB2, 0xE7}, Reading{Extension: 1369, Speed: -6224, Flags: FlagCollisionRecovery}},
	}
	for _, c := range cases {
		got, ok := Parse(c.frame)
		if !ok || got != c.want {
			t.Errorf("%s: Parse(% X) = %+v, %v; want %+v", c.name, c.frame, got, ok, c.want)
		}
		if got.Recovering() != (c.want.Flags&FlagCollisionRecovery != 0) {
			t.Errorf("%s: Recovering() = %v", c.name, got.Recovering())
		}
	}
}

func TestParseRejectsShortFrames(t *testing.T) {
	// Three bytes used to be accepted and then read four.
	for _, f := range [][]byte{nil, {0x01}, {0x01, 0x02, 0x03}} {
		if _, ok := Parse(f); ok {
			t.Errorf("Parse(% X) accepted a frame too short to be a report", f)
		}
	}
}
