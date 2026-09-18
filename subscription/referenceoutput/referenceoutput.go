package referenceoutput

import (
	"encoding/binary"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription"
)

type Subscription struct {
	*subscription.Subscription
}

// Interface
type Subscribable interface {
	AddReferenceOutputCallback(callback func(int, int)) (RemoveCallback func())
}

func NewSubscription(device dpg.Device) Subscribable {
	gatt := dpg.GATT{}
	c, _ := gatt.GetCharacteristic(device, dpg.ServiceUUIDReferenceOutput, dpg.CharacteristicUUIDReferenceOutput)

	return &Subscription{
		subscription.New(c).(*subscription.Subscription),
	}
}

//
// --- CALLBACK WITH PARSING FOR REFERENCEOUTPUT ---
//

func (s *Subscription) AddReferenceOutputCallback(callback func(int, int)) (RemoveCallback func()) {
	return s.AddCallback(subscription.MessageTypeReferenceOutput, func(data []byte) {
		if len(data) < 3 {
			return
		}
		extension := binary.LittleEndian.Uint16(data)
		speed := binary.LittleEndian.Uint16(data[2:])

		callback(int(extension), int(speed))
	})
}
