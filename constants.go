package dpg

// DeskPanel Characteristic Envelope
const (
	EnvelopePrefix     = 127
	EnvelopeDataMarker = 128
)

// The extension byte, present only for commands where NeedsExtensionByte
// reports true. The desk reads it as whether a value follows the envelope,
// and echoes it back as data[2] in the corresponding response.
const (
	extensionNoValue      byte = 0
	extensionValuePresent byte = 1
)

// DeskPanel Characteristic Responses
const (
	DeskPanelResponseCapabilities    = 2
	DeskPanelResponseBaseOffset      = 3
	DeskPanelResponseProductInfo     = 6
	DeskPanelResponseMemoryPosition  = 7
	DeskPanelResponseReminderSetting = 11
	DeskPanelResponseUserID          = 17

	// DeskPanelResponseMemoryPositionUnset is the reply to a request for a
	// memory position holding no height: [1 5 0 <4B counter>].
	//
	// Confirmed on a DPG1M by clearing position 2 - a position the
	// controller certainly has, since its capabilities report two - and
	// asking again. It answered with this code, so 5 means "no height"
	// rather than "no such position".
	//
	// The counter says which kind of empty it is. A position that has been
	// cleared carries a real timestamp, the same ~1 Hz tick the set reply
	// carries; one that has never held a value carries 0xffffffff. That
	// second case cannot be told apart from a position the controller does
	// not have - positions 3 and 4 on a two-position desk answer
	// identically.
	//
	// It is still unknown whether 5 is specific to memory positions or a
	// general "nothing stored" reply - nothing else has been seen to use
	// it.
	DeskPanelResponseMemoryPositionUnset = 5
)

// DeskPanel Characteristic Commands
const (
	DeskPanelCommandProductInfo     DeskPanelCommand = 8
	DeskPanelCommandGetCapabilities DeskPanelCommand = 128
	DeskPanelCommandBaseOffset      DeskPanelCommand = 129
	DeskPanelCommandUserID          DeskPanelCommand = 134
	DeskPanelCommandReminderSetting DeskPanelCommand = 136
	DeskPanelCommandMemoryPosition1 DeskPanelCommand = 137
	DeskPanelCommandMemoryPosition2 DeskPanelCommand = 138
	DeskPanelCommandMemoryPosition3 DeskPanelCommand = 139
	DeskPanelCommandMemoryPosition4 DeskPanelCommand = 140
	DeskPanelCommandGetLogEntry     DeskPanelCommand = 144 // Does nothing on my DPG1M
)

// Control Characteristic Commands
const (
	ControlCommandMoveDown ControlCommand = 70
	ControlCommandMoveUp   ControlCommand = 71
	ControlCommandWakeUp   ControlCommand = 254
	ControlCommandStop     ControlCommand = 255
)

// Capabilities flags
const (
	CapabilitiesFlagMemSize  = 0x07
	CapabilitiesFlagAutoUp   = 0x08
	CapabilitiesFlagAutoDown = 0x10
	CapabilitiesFlagBleAllow = 0x20
	CapabilitiesFlagDisplay  = 0x40
	CapabilitiesFlagLight    = 0x80
)

// Reminder options: the first byte of a reminder settings response, which
// says which of the three presets is active, or that reminders are off.
// The active preset is the reminder cadence. Each preset's two intervals
// are editable from the panel and are not constrained to add up to
// anything in particular: a preset set to stand 60 / sit 20 reads back as
// 20 then 60. The values below are only what the presets ship as, and
// editing one preset leaves the other two alone.
//
// All four were read back from a DPG1M while switching between them on the
// panel, so unlike much of this package these are observed rather than
// inferred. The three interval pairs that follow in the response do not
// change with the selection - the response carries all three presets, not
// just the active one, and they stay put whether reminders are on or off.
//
// Whether the encoding is this enum or a base of 56 with a two-bit
// selector in the low bits is not determined: no value outside 56-59 has
// been seen, and the two readings agree on all of them.
const (
	ReminderOff     ReminderOption = 56 // Reminders off
	ReminderOption1 ReminderOption = 57 // Preset 1, by default 55 sitting / 5 standing
	ReminderOption2 ReminderOption = 58 // Preset 2, by default 50 sitting / 10 standing
	ReminderOption3 ReminderOption = 59 // Preset 3, by default 45 sitting / 15 standing
)

/*
const (
    ReminderOption1DefaultStanding
)

var(
    DefaultReminder Reminder = Reminder {
        Preset1Interval: ReminderInterval{ MinutesStanding: 55, MinutesSitting: 5 },
        Preset2Interval: ReminderInterval{ MinutesStanding: 50, MinutesSitting: 10 },
        Preset3Interval: ReminderInterval{ MinutesStanding: 45, MinutesSitting: 15 },
    }
)
*/
