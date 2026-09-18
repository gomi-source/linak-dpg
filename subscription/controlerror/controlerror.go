package controlerror

import (
	"encoding/binary"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription"
)

type Subscription struct {
	subscription.Subscribable
}

func NewSubscription(device dpg.Device) subscription.Subscribable {
	gatt := dpg.GATT{}
	c, _ := gatt.GetCharacteristic(device, dpg.ServiceUUIDControl, dpg.CharacteristicUUIDControlError)

	return &Subscription{
		subscription.New(c),
	}
}

//
// --- PARSING AND CALLBACK FOR REFERENCEOUTPUT ---
//

func (s *Subscription) AddControlErrorCallback(callback func(int, int)) (RemoveCallback func()) {
	return s.AddCallback(subscription.MessageTypeControlError, func(data []byte) {
		if len(data) < 3 {
			return
		}
		extension := binary.LittleEndian.Uint16(data)
		speed := binary.LittleEndian.Uint16(data[2:])

		callback(int(extension), int(speed))
	})
}
