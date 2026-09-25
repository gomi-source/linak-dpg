// Package controlerror subscribes to the Control service's second
// characteristic, 99FA0003, called "error" after the name LINAK's own
// documentation gives it.
//
// What it carries is known only from what example/moves has made it say
// on a DPG1M. Two kinds of frame have been seen:
//
//   - 01 00 <code>: three bytes, of which the first two have been 01 00
//     every time. The third varies with the cause; see the Code constants.
//   - an empty frame, about 0.8s after an error frame, whether the desk
//     was at rest by then (after a halt) or still backing away (after a
//     collision). It reads as the error clearing. It is not reliable:
//     after one collision none arrived in the 2.1s before the run ended,
//     so nothing should wait for it.
//
// The callback still hands over the raw bytes, and nothing is filtered on
// length: every frame so far has been evidence, and a parse that trimmed
// them to fit what was expected would lose the next surprise. Code and
// Cleared are helpers for reading them, not a gate in front of them.
//
// Frames here are this characteristic's own. A two-byte frame is not a
// write acknowledgement - those belong to DeskPanel alone.
package controlerror

import (
	"fmt"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription"
)

// Codes seen in the third byte, named for the circumstances they appeared
// in rather than for anything LINAK has said about them.
const (
	// CodeInterrupted followed a different height written to a travelling
	// desk, which halts it: 60-90ms after the halting write, every time.
	CodeInterrupted byte = 0x10

	// CodeCollision came as the desk ran into an obstruction, just before
	// its reports showed it stalling and then backing away.
	CodeCollision byte = 0x3B
)

// Code returns the code byte of a 01 00 <code> frame. It returns false for
// any other shape, including an empty frame.
func Code(frame []byte) (byte, bool) {
	if len(frame) < 3 {
		return 0, false
	}
	return frame[2], true
}

// Cleared reports whether a frame is the empty one, which has followed an
// error once the desk was at rest.
func Cleared(frame []byte) bool { return len(frame) == 0 }

// Describe names a code for a log line, or says it has not been seen.
func Describe(code byte) string {
	switch code {
	case CodeInterrupted:
		return "interrupted: a new height reached the desk while it was moving"
	case CodeCollision:
		return "collision: the desk ran into something"
	default:
		return fmt.Sprintf("code 0x%02X, not seen before", code)
	}
}

type Subscription struct {
	*subscription.Subscription
}

// Subscribable is the callback API for the error characteristic.
type Subscribable interface {
	AddControlErrorCallback(callback func([]byte)) (RemoveCallback func())
}

// NewSubscription finds the error characteristic and returns a
// subscription over it.
//
// It returns an error rather than swallowing one, because this
// characteristic is the one that might genuinely be absent: the desk and
// the corebluetoothd helper both have to have it, and no controller other
// than a DPG1M has been checked. A caller that cannot get one should
// carry on without it rather than fail.
//
// A caller that has a desk.Desk should use its ControlErrors rather than
// calling this: Move watches that one, and a subscription's dispatcher can
// never be removed, so a second is a second dispatcher for good.
func NewSubscription(device dpg.Device) (Subscribable, error) {
	gatt := dpg.GATT{}
	c, err := gatt.GetCharacteristic(device, dpg.ServiceUUIDControl, dpg.CharacteristicUUIDControlError)
	if err != nil {
		return nil, fmt.Errorf("control error characteristic: %w", err)
	}

	return &Subscription{
		subscription.New(c).(*subscription.Subscription),
	}, nil
}

// AddControlErrorCallback reports every frame the characteristic emits,
// unparsed and unfiltered.
//
// The callback gets a copy: the notification buffer belongs to the
// dispatch loop and is not the caller's to keep.
func (s *Subscription) AddControlErrorCallback(callback func([]byte)) (RemoveCallback func()) {
	return s.AddCallback(subscription.MessageTypeControlError, func(data []byte) {
		callback(append([]byte(nil), data...))
	})
}
