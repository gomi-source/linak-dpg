package dpg

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

var gattSvcs sync.Map  // key: peripheral ID (string) → []Service
var gattChars sync.Map // key: peripheral ID (string) → []Characteristic

type GATT struct{}

func (g *GATT) Disconnect(device Device) {
	// Remove cached services and characteristics
	gattSvcs.Delete(device.PeripheralID)
	gattChars.Delete(device.PeripheralID)
}

// -----------------------
//   Service discovery
// -----------------------

func (g *GATT) DiscoverServices(device Device) ([]Service, error) {
	// Return cached services if present
	if v, ok := gattSvcs.Load(device.PeripheralID); ok {
		return v.([]Service), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancel()

	// Discover and store
	discovered, err := device.Client.DiscoverServices(ctx, device.PeripheralID, []string{
		ServiceUUIDReferenceOutput,
		ServiceUUIDReferenceInput,
		ServiceUUIDControl,
		ServiceUUIDDeskPanel,
		ServiceUUIDDeviceInformation,
		ServiceUUIDGenericAccess,
	})
	if err != nil {
		return nil, err
	}

	services := make([]Service, len(discovered))
	for i, s := range discovered {
		services[i] = Service{device: device, uuid: strings.ToUpper(s.UUID)}
	}

	gattSvcs.Store(device.PeripheralID, services)

	return services, nil
}

// GetService retrieves a service by UUID, using cache first
func (g *GATT) GetService(device Device, serviceUUID string) (Service, error) {
	// Ensure services are discovered
	services, err := g.DiscoverServices(device)
	if err != nil {
		return Service{}, err
	}

	// Find service in cached slice
	svc, ok := findService(services, serviceUUID)
	if !ok {
		return Service{}, fmt.Errorf("service %v not found for device %v", serviceUUID, device.Address())
	}

	return svc, nil
}

// Find a service in the cached slice
func findService(services []Service, u string) (Service, bool) {
	u = strings.ToUpper(u)
	for _, s := range services {
		if s.uuid == u {
			return s, true
		}
	}
	return Service{}, false
}

// -----------------------
//   Characteristic access
// -----------------------

func (g *GATT) GetCharacteristic(device Device, serviceUUID, characteristicUUID string) (Characteristic, error) {
	characteristicUUID = strings.ToUpper(characteristicUUID)

	// 1. Try from cache
	if v, ok := gattChars.Load(device.PeripheralID); ok {
		if ch, ok := findCharacteristic(v.([]Characteristic), characteristicUUID); ok {
			return ch, nil
		}
	}

	// 2. Discover services if needed
	svc, err := g.GetService(device, serviceUUID)
	if err != nil {
		return Characteristic{}, err
	}

	// 3. Discover characteristic inside this service
	ctx, cancel := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancel()

	chars, err := device.Client.DiscoverCharacteristics(ctx, device.PeripheralID, svc.uuid, []string{characteristicUUID})
	if err != nil {
		return Characteristic{}, err
	}
	if len(chars) == 0 {
		return Characteristic{}, fmt.Errorf("characteristic %v not found", characteristicUUID)
	}

	// 4. Cache it
	char := Characteristic{device: device, serviceUUID: svc.uuid, uuid: strings.ToUpper(chars[0].UUID)}

	g.addCharacteristicToCache(device.PeripheralID, char)

	return char, nil
}

// Append a characteristic to the cache (safe because Max ≈ 10)
func (g *GATT) addCharacteristicToCache(peripheralID string, ch Characteristic) {
	v, _ := gattChars.LoadOrStore(peripheralID, []Characteristic{ch})
	if list, ok := v.([]Characteristic); ok {
		// Append only if not already present
		if _, found := findCharacteristic(list, ch.uuid); found {
			return
		}
		gattChars.Store(peripheralID, append(list, ch))
	}
}

// Finds a characteristic in a cached slice
func findCharacteristic(chars []Characteristic, u string) (Characteristic, bool) {
	u = strings.ToUpper(u)
	for _, c := range chars {
		if c.uuid == u {
			return c, true
		}
	}
	return Characteristic{}, false
}
