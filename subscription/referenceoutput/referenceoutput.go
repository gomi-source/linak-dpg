// Package referenceoutput follows the desk's position reports: the
// ReferenceOutput characteristic, 99FA0021, which the desk pushes while it
// moves and not at all while it stands still.
//
// A report is four bytes: the extension, then a speed word, both
// little-endian.
//
// The speed word is not only a speed. In every ordinary move seen so far
// its bottom four bits are zero - speeds come in steps of 16, full speed
// being ±6224 (0x1850) - but when a DPG1M ran into an obstruction, every
// report from that moment until it came to rest again had bit 1 set: a
// stall reported as 02 00, and the drive back away from the obstruction
// as 0x0262, 0x1852 and so on, until a final 00 00. So the bottom four bits
// are treated as flags and kept out of the speed. That rests on one blocked
// run, so the raw bits are passed on in full rather than only the one that
// has been seen, and Speed is what is left once all four are removed.
package referenceoutput

import (
	"encoding/binary"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription"
)

// FlagCollisionRecovery is set in every report from the moment a desk runs
// into something until it has finished backing away from it. Observed on a
// DPG1M; see the package doc.
const FlagCollisionRecovery uint8 = 0x2

// flagBits are the bits of the speed word that carry flags, not speed.
const flagBits = 0x000F

// Reading is one position report.
type Reading struct {
	// Extension is the height in tenths of a millimetre above the desk's
	// own base.
	Extension int

	// Speed is signed - negative going down - with the flag bits removed.
	Speed int

	// Flags are the bottom four bits of the speed word, as they arrived.
	Flags uint8
}

// Recovering reports whether the desk is in its collision recovery: it has
// hit something and is stopping or backing away, and is not following the
// heights written to it.
func (r Reading) Recovering() bool { return r.Flags&FlagCollisionRecovery != 0 }

// Parse reads one report. It returns false for a frame too short to be
// one.
func Parse(data []byte) (Reading, bool) {
	if len(data) < 4 {
		return Reading{}, false
	}
	raw := binary.LittleEndian.Uint16(data[2:])
	return Reading{
		Extension: int(binary.LittleEndian.Uint16(data)),
		// Signed: read unsigned, a desk descending at full speed reported
		// 59312 where it means -6224. The flags are cleared before the
		// conversion, so a flagged report going down keeps its sign.
		Speed: int(int16(raw &^ flagBits)),
		Flags: uint8(raw & flagBits),
	}, true
}

type Subscription struct {
	*subscription.Subscription
}

// Subscribable is the callback API for position reports.
type Subscribable interface {
	// AddReferenceOutputCallback reports extension and speed. The speed has
	// the flag bits removed; use AddReadingCallback to see them.
	AddReferenceOutputCallback(callback func(extension, speed int)) (RemoveCallback func())

	// AddReadingCallback reports each report whole, flags included.
	AddReadingCallback(callback func(Reading)) (RemoveCallback func())
}

func NewSubscription(device dpg.Device) Subscribable {
	gatt := dpg.GATT{}
	c, _ := gatt.GetCharacteristic(device, dpg.ServiceUUIDReferenceOutput, dpg.CharacteristicUUIDReferenceOutput)

	return &Subscription{
		subscription.New(c).(*subscription.Subscription),
	}
}

func (s *Subscription) AddReadingCallback(callback func(Reading)) (RemoveCallback func()) {
	return s.AddCallback(subscription.MessageTypeReferenceOutput, func(data []byte) {
		if r, ok := Parse(data); ok {
			callback(r)
		}
	})
}

func (s *Subscription) AddReferenceOutputCallback(callback func(int, int)) (RemoveCallback func()) {
	return s.AddReadingCallback(func(r Reading) {
		callback(r.Extension, r.Speed)
	})
}
