package dpg

import (
	"encoding/base64"
	"strings"
	"sync"

	"github.com/gomi-source/corebluetooth-go/ble"
)

// notifyKey identifies one GATT characteristic's notification stream.
type notifyKey struct {
	peripheralID       string
	serviceUUID        string
	characteristicUUID string
}

// clientNotifyRegistry demuxes one *ble.Client's single shared
// Notifications() channel out to per-characteristic callbacks.
// tinygo.org/x/bluetooth delivered notifications already split per
// characteristic at the OS binding layer; corebluetoothd's JSON-RPC bridge
// exposes one shared event stream per Client instead (see PROTOCOL.md), so
// this fans it back out - one goroutine per Client, started lazily the
// first time any Characteristic on it enables notifications.
type clientNotifyRegistry struct {
	mu        sync.Mutex
	callbacks map[notifyKey][]func([]byte)
}

var (
	registriesMu sync.Mutex
	registries   = map[*ble.Client]*clientNotifyRegistry{}
)

func notifyRegistryFor(client *ble.Client) *clientNotifyRegistry {
	registriesMu.Lock()
	defer registriesMu.Unlock()

	if r, ok := registries[client]; ok {
		return r
	}

	r := &clientNotifyRegistry{callbacks: make(map[notifyKey][]func([]byte))}
	registries[client] = r

	go func() {
		for update := range client.Notifications() {
			data, err := base64.StdEncoding.DecodeString(update.ValueBase64)
			if err != nil {
				continue
			}
			key := notifyKey{
				peripheralID:       update.PeripheralID,
				serviceUUID:        strings.ToUpper(update.ServiceUUID),
				characteristicUUID: strings.ToUpper(update.CharacteristicUUID),
			}

			r.mu.Lock()
			cbs := append([]func([]byte){}, r.callbacks[key]...)
			r.mu.Unlock()

			for _, cb := range cbs {
				cb(data)
			}
		}
	}()

	return r
}

func (r *clientNotifyRegistry) subscribe(key notifyKey, callback func([]byte)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.callbacks[key] = append(r.callbacks[key], callback)
}
