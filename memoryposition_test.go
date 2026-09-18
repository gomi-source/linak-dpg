package dpg

import (
	"bytes"
	"testing"
)

// The wire formats here are what a DPG1M was observed to accept and report back
func TestMemoryPositionEnvelope(t *testing.T) {
	ext := 4800

	tests := []struct {
		name string
		m    MemoryPosition
		want []byte
	}{
		{
			"set position 2 to 4800",
			MemoryPosition{Position: 2, Extension: &ext},
			[]byte{EnvelopePrefix, 138, EnvelopeDataMarker, 0x01, 0xC0, 0x12, 0, 0, 0, 0},
		},
		{
			"clear position 2",
			MemoryPosition{Position: 2},
			[]byte{EnvelopePrefix, 138, EnvelopeDataMarker, 0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.m.Envelope()
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestMemoryPositionExtensionByte(t *testing.T) {
	ext := 1300
	if got := (MemoryPosition{Position: 1, Extension: &ext}).extensionByte(); got != extensionValuePresent {
		t.Errorf("extension byte for a set position = %d, want %d", got, extensionValuePresent)
	}
	if got := (MemoryPosition{Position: 1}).extensionByte(); got != extensionNoValue {
		t.Errorf("extension byte for a cleared position = %d, want %d", got, extensionNoValue)
	}
}

// Base offset keeps the extension byte the envelope supplies, and its
// payload is the bare height - the arrangement memory positions now match.
func TestBaseOffsetEnvelopeUnchanged(t *testing.T) {
	h := NewHeight(5500).Bytes()
	got := EnvelopeWithPayload(DeskPanelCommandBaseOffset, h[:])
	want := []byte{EnvelopePrefix, 129, EnvelopeDataMarker, 0x01, 0x7C, 0x15}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

// A command that takes no extension byte must not gain one.
func TestEnvelopeWithoutExtensionByte(t *testing.T) {
	got := EnvelopeWithPayload(DeskPanelCommandUserID, []byte{0x01, 0x33})
	want := []byte{EnvelopePrefix, 134, EnvelopeDataMarker, 0x01, 0x33}
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}
