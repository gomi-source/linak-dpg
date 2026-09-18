package subscription

import (
	"sync"

	"github.com/gomi-source/linak-dpg"
)

type MessageType uint8

const (
	MessageTypeWriteResponse            MessageType = 255 // Common to all writable characteristics? Or just DPG?
	MessageTypeControlError             MessageType = 254
	MessageTypeReferenceOutput          MessageType = 1
	MessageTypeDeskPanelCapabilities    MessageType = dpg.DeskPanelResponseCapabilities
	MessageTypeDeskPanelBaseOffset      MessageType = dpg.DeskPanelResponseBaseOffset
	MessageTypeDeskPanelMemoryPosition  MessageType = dpg.DeskPanelResponseMemoryPosition
	MessageTypeDeskPanelMemoryUnset     MessageType = dpg.DeskPanelResponseMemoryPositionUnset
	MessageTypeDeskPanelReminderSetting MessageType = dpg.DeskPanelResponseReminderSetting
	MessageTypeDeskPanelProductInfo     MessageType = dpg.DeskPanelResponseProductInfo
	MessageTypeDeskPanelUserID          MessageType = dpg.DeskPanelResponseUserID
)

type Subscription struct {
	characteristic      dpg.Characteristic
	ch                  chan []byte
	mu                  sync.Mutex
	started             bool
	registeredCallbacks map[byte]map[int]func([]byte)
	nextCallbackId      int
}

// Interface
type Subscribable interface {
	AddWriteCallback(callback func([2]byte)) (RemoveCallback func())
	AddCallback(MessageType, func([]byte)) (RemoveCallback func())
}

// Constructor
func New(characteristic dpg.Characteristic) Subscribable {
	return &Subscription{
		characteristic:      characteristic,
		ch:                  make(chan []byte, 200),
		registeredCallbacks: make(map[byte]map[int]func([]byte)),
		nextCallbackId:      1,
	}
}

// All characteristics, and therefore all implementers of this interface, have the WriteCallback
func (s *Subscription) AddWriteCallback(callback func([2]byte)) (RemoveCallback func()) {
	return s.AddCallback(MessageTypeWriteResponse, func(data []byte) {
		if len(data) != 2 {
			return
		}
		callback([2]byte{data[0], data[1]})
	})
}

// Generic AddCallback function
func (s *Subscription) AddCallback(msgType MessageType, callback func([]byte)) func() {
	if s.registeredCallbacks[byte(msgType)] == nil {
		s.registeredCallbacks[byte(msgType)] = make(map[int]func([]byte))
	}

	s.nextCallbackId++
	id := s.nextCallbackId
	s.registeredCallbacks[byte(msgType)][id] = callback

	s.start()

	return func() {
		delete(s.registeredCallbacks[byte(msgType)], id)
	}
}

// Enable notifications on DeskPanel Characteristic and start dispatch loop
func (s *Subscription) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	s.started = true

	// Enable notifications on characteristic
	if err := s.characteristic.EnableNotifications(func(buf []byte) {
		s.ch <- buf
	}); err != nil {
		return err
	}

	dispatchLoop := func() {
		for data := range s.ch {
			var msgType byte
			switch s.characteristic.UUID() {
			case dpg.CharacteristicUUIDDeskPanel:
				if len(data) < 2 {
					continue
				}

				// Check if valid data from DeskPanel
				if data[0] != 1 {
					continue
				}
				msgType = data[1]
			case dpg.CharacteristicUUIDReferenceOutput:
				// ReferenceOutput has only one message type
				msgType = byte(MessageTypeReferenceOutput)
			}

			if callbacks, ok := s.registeredCallbacks[msgType]; ok {
				for _, handler := range callbacks {
					go handler(data)
				}
			}

			// Write responses are common to all characteristics and always length 2
			if len(data) == 2 {
				msgType = byte(MessageTypeWriteResponse)
				if callbacks, ok := s.registeredCallbacks[msgType]; ok {
					for _, handler := range callbacks {
						go handler(data)
					}
				}
			}
		}
	}

	go dispatchLoop()
	return nil
}
