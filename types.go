package dpg

import (
	"encoding/binary"
	"encoding/hex"
)

type DeskPanelCommand uint8
type ControlCommand uint8
type ReminderOption uint8

type Capabilities struct {
	MemSize    int
	AutoUp     bool
	AutoDown   bool
	BleAllow   bool
	HasDisplay bool
	HasLight   bool
}

type ProductInfo struct {
	// this is guesswork, no idea what the data actually represents
	ProductCode     int
	HardwareVersion int
	SoftwareVersion int
}

type ReminderIntervals struct {
	MinutesStanding int
	MinutesSitting  int
}

type Reminder struct {
	ActiveOption ReminderOption
	Option1      ReminderIntervals
	Option2      ReminderIntervals
	Option3      ReminderIntervals
}

type Height struct {
	*int
}

type Favourite struct {
	*int
}

type MemoryPosition struct {
	Position  int
	Extension *int
}

type ReferenceOutput struct {
	Extension int
	Speed     int
}

type User struct {
	ID   string
	Data []byte
}

// Create the envelope used to send commands to DeskPanel Characteristic
func Envelope(cmd DeskPanelCommand) []byte {
	envelope := make([]byte, 3)
	envelope[0] = EnvelopePrefix
	envelope[1] = byte(cmd)
	envelope[2] = 0 // terminates the query, signifying this is a read command and no data will follow
	return envelope
}

// Create the envelope used to send commands to DeskPanel Characteristic to overwrite characteristic data.
//
// The extension byte accompanies a height: 1 when one follows, 0 when none
// does. Both commands known to take it - base offset and the four memory
// positions - write a 16-bit mmx10 height, and user ID writes a value
// without it, so carrying data is not what triggers it. Whether that
// generalises to heights specifically, or is just these five commands, is
// untested; reminder settings are the obvious case to check, and nothing
// in this package writes them yet.
//
// This supplies 1, which is all base offset ever needs.
// MemoryPosition.Envelope covers the one case that has to say 0.
func EnvelopeWithPayload(cmd DeskPanelCommand, payload []byte) []byte {
	return envelopeWithExtensionByte(cmd, extensionValuePresent, payload)
}

// envelopeWithExtensionByte is EnvelopeWithPayload with the extension byte
// chosen by the caller. It is unexported because the value is not a free
// choice: it follows from what is being written, so it belongs to the type
// being written rather than to the call site.
//
// Not to be confused with EnvelopeDataMarker, which is the fixed byte at
// index 2 saying the envelope carries data at all. The extension byte sits
// after it, at index 3, and only for the commands that take one - it is
// ignored for the rest.
func envelopeWithExtensionByte(cmd DeskPanelCommand, extensionByte byte, payload []byte) []byte {
	extra := 0
	if cmd.NeedsExtensionByte() {
		extra = 1
	}
	n := 3 + extra
	envelope := make([]byte, n+len(payload))
	envelope[0] = EnvelopePrefix
	envelope[1] = byte(cmd)
	envelope[2] = EnvelopeDataMarker // signal that payload for overwriting data on device follows
	if extra == 1 {
		envelope[3] = extensionByte
	}
	copy(envelope[n:], payload)
	return envelope
}

// NeedsExtensionByte reports whether this command's payload is preceded by
// an extension byte. It says only that the byte is there - the value
// depends on what is being written rather than on the command, so it comes
// from the value being packed.
func (cmd DeskPanelCommand) NeedsExtensionByte() bool {
	switch cmd {
	case DeskPanelCommandBaseOffset, DeskPanelCommandMemoryPosition1, DeskPanelCommandMemoryPosition2,
		DeskPanelCommandMemoryPosition3, DeskPanelCommandMemoryPosition4:
		return true
	}
	return false
}

// Bytes packs a control command for the Control characteristic, which
// takes a little-endian 16-bit value - the command byte followed by a
// zero. So wake-up (254) goes out as FE 00 and stop (255) as FF 00.
func (cmd ControlCommand) Bytes() []byte {
	return []byte{byte(cmd), 0x00}
}

// Height constructor
func NewHeight(extension int) *Height {
	return &Height{&extension}
}

// Height method to pack base offset data as binary byte array before writing to Characteristic
func (bo Height) Bytes() [2]byte {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[0:], uint16(*bo.int))
	return b
}
func (m MemoryPosition) IsValid() bool {
	switch {
	case m.Position < 1 || m.Position > 4:
		return false
	case m.Extension != nil && *m.Extension < 0:
		return false
	}

	return true
}

func (m MemoryPosition) Command() DeskPanelCommand {
	switch m.Position {
	case 2:
		return DeskPanelCommandMemoryPosition2
	case 3:
		return DeskPanelCommandMemoryPosition3
	case 4:
		return DeskPanelCommandMemoryPosition4
	default:
		return DeskPanelCommandMemoryPosition1
	}
}

// Envelope builds the whole DeskPanel write for this position: the one
// that stores its height, or, when Extension is nil, the one that clears
// the slot.
//
// This is the only write whose extension byte is ever 0, which is why the
// packing lives here rather than being assembled by the caller out of
// Command, extensionByte and Bytes.
func (m MemoryPosition) Envelope() []byte {
	return envelopeWithExtensionByte(m.Command(), m.extensionByte(), m.Bytes())
}

// extensionByte precedes the payload: a value follows when a height is
// set, none when the position is being cleared.
func (m MemoryPosition) extensionByte() byte {
	if m.Extension == nil {
		return extensionNoValue
	}
	return extensionValuePresent
}

// Bytes packs the payload that follows the extension byte - that byte is
// not included, because the envelope carries it. Including it here as well
// shifts everything by one, and the desk then stores the extension byte
// plus the first height byte as the height.
func (m MemoryPosition) Bytes() []byte {
	if !m.IsValid() {
		return nil
	}

	if m.Extension == nil {
		// Clearing a position: a 0xffffffff counter, with the extension
		// byte (none) supplied by the envelope.
		b := make([]byte, 6)
		binary.LittleEndian.PutUint32(b, 0xffffffff)
		return b
	}

	// Setting one: [2B height][4B counter]. The counter is maintained by
	// the desk - it reports its own value regardless of what is written
	// here - so the allocated zeroes are left alone.
	b := make([]byte, 6)
	binary.LittleEndian.PutUint16(b, uint16(*m.Extension))
	return b
}

// Reminder method to pack favourite data as binary byte array before writing to Characteristic
// IsValid reports whether these settings can be packed without losing
// information. Each interval goes out as a single byte, so a value outside
// 0-255 would be silently truncated, and the active option has to be one
// the desk recognises - writing an unknown one puts it in a state nothing
// here has explored.
func (r Reminder) IsValid() bool {
	switch r.ActiveOption {
	case ReminderOff, ReminderOption1, ReminderOption2, ReminderOption3:
	default:
		return false
	}
	for _, i := range []ReminderIntervals{r.Option1, r.Option2, r.Option3} {
		if i.MinutesSitting < 0 || i.MinutesSitting > 255 {
			return false
		}
		if i.MinutesStanding < 0 || i.MinutesStanding > 255 {
			return false
		}
	}
	return true
}

// Bytes packs reminder settings for the DeskPanel characteristic: the
// active option, then each preset as sitting minutes followed by standing
// minutes, then four bytes for the counter the desk maintains itself.
//
// Sitting comes first in each pair, which is the opposite of what the
// field order suggests. Confirmed directly: with preset 1 set from LINAK's
// app to stand 60 / sit 20, the desk reports that preset as 20 then 60.
// The app presents the pair the other way round, standing first, which is
// the likely reason these were once read in that order.
//
// Confirmed in both directions against a DPG1M: writing preset 1 as 23
// sitting / 61 standing through Desk.WriteReminder made the desk report it
// back as 23 then 61, and leave the other two presets alone.
func (r Reminder) Bytes() [11]byte {
	var b = [11]byte{
		byte(r.ActiveOption),
		byte(r.Option1.MinutesSitting),
		byte(r.Option1.MinutesStanding),
		byte(r.Option2.MinutesSitting),
		byte(r.Option2.MinutesStanding),
		byte(r.Option3.MinutesSitting),
		byte(r.Option3.MinutesStanding),
		0, 0, 0, 0}
	return b
}

// Constructor that parses RefereceOutput data format
func NewReferenceOutput(data []byte) *ReferenceOutput {
	if len(data) < 4 {
		return &ReferenceOutput{}
	}

	extension := binary.LittleEndian.Uint16(data)
	speed := binary.LittleEndian.Uint16(data[2:])
	return &ReferenceOutput{
		Extension: int(extension),
		Speed:     int(speed),
	}
}

func NewUser(data []byte) *User {
	return &User{
		Data: data,
		ID:   hex.EncodeToString(data),
	}
}

// Owner reports whether this user ID is marked as the desk's owner. The
// desk ignores writes - including the ReferenceInput writes that move it -
// from a user whose owner bit is clear.
func (u *User) Owner() bool {
	return len(u.Data) > 0 && u.Data[0] == 1
}

func (u *User) SetOwner(isOwner bool) {
	if len(u.Data) == 0 {
		return
	}
	if isOwner {
		u.Data[0] = 1
		return
	}
	u.Data[0] = 0
}
