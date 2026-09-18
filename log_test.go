package dpg

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestDeviceLoggerDefaultsToDiscarding(t *testing.T) {
	var d Device
	if d.Logger() == nil {
		t.Fatal("Logger must never return nil")
	}
	// A discarding logger reports debug as disabled, which is what keeps
	// callers from paying to format messages nobody reads.
	if d.Logger().Enabled(t.Context(), slog.LevelDebug) {
		t.Error("the default logger should discard, not record")
	}
}

func TestWithLoggerReturnsACopy(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	original := NewDevice(nil, "peripheral-1")
	logged := original.WithLogger(l)

	if original.Logger().Enabled(t.Context(), slog.LevelDebug) {
		t.Error("WithLogger must not modify the Device it was called on")
	}

	logged.Logger().Debug("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("the returned Device should log through the given logger, got %q", buf.String())
	}
	if logged.PeripheralID != original.PeripheralID {
		t.Error("WithLogger must preserve the rest of the Device")
	}
}

func TestHexBytes(t *testing.T) {
	if got := hexBytes([]byte{0x7F, 0x8A, 0x00}); got != "7F 8A 00" {
		t.Errorf("hexBytes = %q", got)
	}
	if got := hexBytes(nil); got != "(empty)" {
		t.Errorf("hexBytes(nil) = %q", got)
	}
}

func TestShortUUID(t *testing.T) {
	if got := ShortUUID(CharacteristicUUIDDeskPanel); got != "99FA0011" {
		t.Errorf("ShortUUID = %q, want the distinguishing first group", got)
	}
	if got := ShortUUID("no-dashes"); got != "no" {
		t.Errorf("ShortUUID = %q", got)
	}
}
