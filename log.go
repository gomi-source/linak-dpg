package dpg

import (
	"fmt"
	"log/slog"
	"strings"
)

// discard is what a Device logs through until a caller opts in. A library
// that writes to somebody's output on its own initiative is a nuisance,
// so nothing is logged by default and nothing is ever written to stdout.
var discard = slog.New(slog.DiscardHandler)

// WithLogger returns a copy of the Device that logs every GATT write and
// every notification at debug level.
//
// Device is a value type, so use the returned copy rather than the
// original:
//
//	device := dpg.NewDevice(client, id).WithLogger(log)
//	d := desk.New(device, "office")
//
// Everything built from that Device logs through the same logger: its
// characteristics, its subscriptions, and the desk package.
func (d Device) WithLogger(l *slog.Logger) Device {
	d.logger = l
	return d
}

// Logger returns the Device's logger, or one that discards everything if
// none was set. It never returns nil.
func (d Device) Logger() *slog.Logger {
	if d.logger == nil {
		return discard
	}
	return d.logger
}

// Logger returns the logger of the Device this characteristic belongs to.
func (c Characteristic) Logger() *slog.Logger { return c.device.Logger() }

// hexBytes renders a payload for a log line. Bytes are what these logs
// are for, so they are spelled out rather than summarised.
func hexBytes(b []byte) string {
	if len(b) == 0 {
		return "(empty)"
	}
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, " ")
}

// ShortUUID trims a 128-bit UUID to its first group, which is the part
// that tells the LINAK services and characteristics apart - the remaining
// 96 bits are the same for all of them.
func ShortUUID(u string) string {
	if i := strings.IndexByte(u, '-'); i > 0 {
		return u[:i]
	}
	return u
}
