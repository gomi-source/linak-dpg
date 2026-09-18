package deskpanel

import (
	"encoding/binary"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription"
)

type Subscription struct {
	subscription.Subscribable
}

// Subscribable is the DeskPanel characteristic's typed notification API:
// the generic subscription plus one parsing callback per DeskPanel
// response. Returning this from NewSubscription rather than the bare
// subscription.Subscribable saves every caller a type assertion to get at
// the parsed callbacks.
type Subscribable interface {
	subscription.Subscribable
	AddCapabilitiesCallback(callback func(dpg.Capabilities)) (RemoveCallback func())
	AddBaseOffsetCallback(callback func(int)) (RemoveCallback func())
	AddMemoryPositionCallback(callback func(int)) (RemoveCallback func())
	AddMemoryPositionUnsetCallback(callback func()) (RemoveCallback func())
	AddReminderSettingCallback(callback func(dpg.Reminder)) (RemoveCallback func())
	AddProductInfoCallback(callback func(dpg.ProductInfo)) (RemoveCallback func())
	AddUserIDCallback(callback func([]byte)) (RemoveCallback func())
}

func NewSubscription(device dpg.Device) Subscribable {
	gatt := dpg.GATT{}
	c, _ := gatt.GetCharacteristic(device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)

	return &Subscription{
		subscription.New(c),
	}
}

//
// --- CALLBACK WITH PARSING FOR EACH DESKPANEL PARAMETER ---
//

func (s *Subscription) AddCapabilitiesCallback(callback func(dpg.Capabilities)) func() {
	return s.AddCallback(dpg.DeskPanelResponseCapabilities, func(data []byte) {
		if len(data) < 4 {
			return
		}

		b := data[2]
		r := dpg.Capabilities{
			MemSize:    int(b & dpg.CapabilitiesFlagMemSize),
			AutoUp:     b&dpg.CapabilitiesFlagAutoUp != 0,
			AutoDown:   b&dpg.CapabilitiesFlagAutoDown != 0,
			BleAllow:   b&dpg.CapabilitiesFlagBleAllow != 0,
			HasDisplay: b&dpg.CapabilitiesFlagDisplay != 0,
			HasLight:   b&dpg.CapabilitiesFlagLight != 0,
		}

		callback(r)
	})
}

func (s *Subscription) AddBaseOffsetCallback(callback func(int)) func() {
	return s.AddCallback(dpg.DeskPanelResponseBaseOffset, func(data []byte) {
		// e.g. [1 3 1 24 21]
		if len(data) < 5 {
			return
		}
		r := binary.LittleEndian.Uint16(data[3:5])
		callback(int(r))
	})
}

func (s *Subscription) AddMemoryPositionCallback(callback func(int)) (RemoveCallback func()) {
	return s.AddCallback(dpg.DeskPanelResponseMemoryPosition, func(data []byte) {
		// e.g. [1 7 1 192 18 93 23 246 5]
		if len(data) < 4 {
			return
		}
		extension := binary.LittleEndian.Uint16(data[3:])
		callback(int(extension))
	})
}

// AddMemoryPositionUnsetCallback fires when the desk answers a memory
// position request with no height. It arrives under its own response code
// rather than as a short DeskPanelResponseMemoryPosition, so without this
// callback such a position produces no notification at all.
//
// The callback takes no arguments because the reply says nothing about
// which position was asked about - pairing a reply with its request means
// issuing one request at a time and waiting for the answer. The reply does
// carry a counter distinguishing a position that was cleared from one that
// never held a value (see dpg.DeskPanelResponseMemoryPositionUnset); pass
// it through here if that distinction becomes useful.
func (s *Subscription) AddMemoryPositionUnsetCallback(callback func()) (RemoveCallback func()) {
	return s.AddCallback(dpg.DeskPanelResponseMemoryPositionUnset, func(data []byte) {
		// e.g. [1 5 0 255 255 255 255]
		if len(data) < 3 || data[2] != 0 {
			return
		}
		callback()
	})
}

func (s *Subscription) AddReminderSettingCallback(callback func(dpg.Reminder)) func() {
	return s.AddCallback(dpg.DeskPanelResponseReminderSetting, func(data []byte) {
		r, ok := parseReminder(data)
		if !ok {
			return
		}
		callback(r)
	})
}

// parseReminder decodes a reminder settings response:
//
//	[1 11 56 55 5 50 10 45 15 <4B counter>]
//
// the active option, then three presets as sitting minutes followed by
// standing minutes, then a counter the desk maintains for itself (it
// reports its own value whatever is written there, and updates it on every
// write).
//
// Sitting comes first in each pair, which is the opposite of what the
// field order suggests. Confirmed directly: with preset 1 set from LINAK's
// app to stand 60 / sit 20, the desk reports that preset as 20 then 60.
// The app presents the pair the other way round, standing first, which is
// the likely reason these were once read in that order.
//
// Confirmed against a DPG1M across all four values of the active option,
// and in the write direction too - a preset written as 23 sitting / 61
// standing reads back as 23 then 61.
// The shipped defaults are 55/5, 50/10 and 45/15, but both intervals of
// every preset are editable and bear no fixed relationship to each other -
// 20/60 is a legal setting, and editing one preset leaves the other two
// alone. Split out from the callback so those frames can be pinned by a
// test.
func parseReminder(data []byte) (dpg.Reminder, bool) {
	if len(data) < 9 {
		return dpg.Reminder{}, false
	}
	return dpg.Reminder{
		ActiveOption: dpg.ReminderOption(data[2]),
		Option1:      dpg.ReminderIntervals{MinutesSitting: int(data[3]), MinutesStanding: int(data[4])},
		Option2:      dpg.ReminderIntervals{MinutesSitting: int(data[5]), MinutesStanding: int(data[6])},
		Option3:      dpg.ReminderIntervals{MinutesSitting: int(data[7]), MinutesStanding: int(data[8])},
	}, true
}

func (s *Subscription) AddProductInfoCallback(callback func(dpg.ProductInfo)) func() {
	return s.AddCallback(dpg.DeskPanelResponseProductInfo, func(data []byte) {
		// e.g. [1 6 32 25 8 20 132 13]
		if len(data) < 6 {
			return
		}
		r := dpg.ProductInfo{
			ProductCode:     int(binary.LittleEndian.Uint16(data[3:5])),
			HardwareVersion: int(data[5]),
			SoftwareVersion: int(data[6]),
		}
		callback(r)
	})
}

func (s *Subscription) AddUserIDCallback(callback func([]byte)) func() {
	return s.AddCallback(dpg.DeskPanelResponseUserID, func(data []byte) {
		// e.g. [1 17 1 51 251 186 ...]: the envelope's two bytes, then the
		// user ID itself, whose first byte is the owner flag.
		if len(data) < 3 {
			return
		}
		callback(data[2:])
	})
}
