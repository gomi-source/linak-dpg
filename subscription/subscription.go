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

// AddCallback registers a handler for one message type and returns a
// function that removes it again.
//
// It is safe to call from any goroutine, and safe to call from inside a
// handler: the callback map is only ever touched under s.mu, and the
// dispatch loop copies the handlers it is about to run rather than ranging
// over the map while it calls them. Without that, adding a callback while
// a notification was being dispatched was not merely a data race - a map
// written during a range over it is a fatal runtime error, which would
// have taken the process down rather than corrupting a reading.
//
// Removing a handler stops it being called. It does not stop the
// subscription: once started, its dispatcher lives for as long as the
// client does. See the note on that in the package README.
func (s *Subscription) AddCallback(msgType MessageType, callback func([]byte)) func() {
	key := byte(msgType)
	id := s.register(key, callback)

	// Deliberately outside the lock: start takes s.mu itself, and
	// EnableNotifications reaches the BLE client, which should never be
	// called while holding a lock the dispatch loop needs.
	s.start()

	return func() { s.unregister(key, id) }
}

// register stores a handler and returns the id that removes it.
func (s *Subscription) register(key byte, callback func([]byte)) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.registeredCallbacks == nil {
		s.registeredCallbacks = make(map[byte]map[int]func([]byte))
	}
	if s.registeredCallbacks[key] == nil {
		s.registeredCallbacks[key] = make(map[int]func([]byte))
	}

	s.nextCallbackId++
	id := s.nextCallbackId
	s.registeredCallbacks[key][id] = callback
	return id
}

// unregister drops a handler. Removing one twice, or removing from a
// message type that has none, is a no-op.
func (s *Subscription) unregister(key byte, id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.registeredCallbacks[key], id)
}

// handlersFor copies the handlers registered for a message type, so they
// can be called without holding the lock - and so that one of them adding
// or removing a callback cannot mutate the map being iterated.
func (s *Subscription) handlersFor(key byte) []func([]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	registered := s.registeredCallbacks[key]
	if len(registered) == 0 {
		return nil
	}
	handlers := make([]func([]byte), 0, len(registered))
	for _, h := range registered {
		handlers = append(handlers, h)
	}
	return handlers
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

			for _, handler := range s.handlersFor(msgType) {
				go handler(data)
			}

			// Write responses are common to all characteristics and always length 2
			if len(data) == 2 {
				for _, handler := range s.handlersFor(byte(MessageTypeWriteResponse)) {
					go handler(data)
				}
			}
		}
	}

	go dispatchLoop()
	return nil
}
