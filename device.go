package dpg

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gomi-source/corebluetooth-go/ble"
)

// defaultRPCTimeout bounds every corebluetoothd round trip made from this
// package. tinygo.org/x/bluetooth's Device/Characteristic methods were
// synchronous with no caller-supplied timeout, so this package's own
// exported API (Desk, GATT, Subscription, ...) still takes none either -
// this is the closest equivalent: generous for a nearby BLE peripheral,
// but bounded instead of hanging forever if the desk goes out of range.
const defaultRPCTimeout = 10 * time.Second

// Device replaces tinygo.org/x/bluetooth.Device: a connected peripheral,
// addressed by its CoreBluetooth peripheral ID and reached through a
// shared *ble.Client (see github.com/gomi-source/corebluetooth-go). This
// package only speaks GATT - scanning and connecting happen wherever the
// Client comes from; hand the resulting *ble.Client and peripheral ID here
// via NewDevice once connected.
type Device struct {
	Client       *ble.Client
	PeripheralID string

	// logger is unset by default, which means nothing is logged. Set it
	// with WithLogger; read it with Logger, which never returns nil.
	logger *slog.Logger
}

// NewDevice constructs a Device from a *ble.Client and the peripheral ID
// returned by that client's Discoveries() / used with Connect().
func NewDevice(client *ble.Client, peripheralID string) Device {
	return Device{Client: client, PeripheralID: peripheralID}
}

// IsZero reports whether this is the zero Device (mirrors the old
// `device == (bluetooth.Device{})` check in desk.New).
func (d Device) IsZero() bool {
	return d.Client == nil || d.PeripheralID == ""
}

// Address mirrors the `device.Address.String()` call sites that used
// tinygo's bluetooth.Device. Note: unlike a MAC address, a CoreBluetooth
// peripheral ID is stable across reconnects on the same Mac, but a
// *different* Mac will see a different ID for the same physical desk.
func (d Device) Address() string {
	return d.PeripheralID
}

// Service is a discovered GATT service on a Device.
type Service struct {
	device Device
	uuid   string // canonical upper-case form, e.g. "99FA0001-338A-1024-8A49-009C0215F78A"
}

func (s Service) UUID() string { return s.uuid }

// Characteristic is a discovered GATT characteristic on a Service. Its
// Write/WriteWithoutResponse/EnableNotifications methods mirror
// tinygo.org/x/bluetooth.DeviceCharacteristic's signatures closely enough
// that call sites elsewhere in this module needed no changes.
type Characteristic struct {
	device      Device
	serviceUUID string
	uuid        string // canonical upper-case form
}

func (c Characteristic) UUID() string { return c.uuid }

// Write performs a write-with-response, blocking until the peripheral
// acknowledges it.
func (c Characteristic) Write(data []byte) (int, error) {
	log := c.Logger().With(
		"characteristic", ShortUUID(c.uuid),
		"bytes", hexBytes(data),
		"with_response", true)
	log.Debug("gatt write")

	ctx, cancel := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancel()
	if err := c.device.Client.WriteCharacteristic(ctx, c.device.PeripheralID, c.serviceUUID, c.uuid, data, true); err != nil {
		log.Debug("gatt write failed", "err", err)
		return 0, err
	}
	return len(data), nil
}

// WriteWithoutResponse queues a write and returns as soon as it's sent;
// CoreBluetooth never acknowledges this write type.
func (c Characteristic) WriteWithoutResponse(data []byte) (int, error) {
	log := c.Logger().With(
		"characteristic", ShortUUID(c.uuid),
		"bytes", hexBytes(data),
		"with_response", false)
	log.Debug("gatt write")

	ctx, cancel := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancel()
	if err := c.device.Client.WriteCharacteristic(ctx, c.device.PeripheralID, c.serviceUUID, c.uuid, data, false); err != nil {
		log.Debug("gatt write failed", "err", err)
		return 0, err
	}
	return len(data), nil
}

// EnableNotifications subscribes to this characteristic's notify/indicate
// updates and invokes callback for each one. Unlike tinygo's backend,
// corebluetoothd delivers all of a Client's characteristic updates on one
// shared channel (see PROTOCOL.md); notify.go demuxes that back out to the
// right callback per (peripheral, service, characteristic), so this method
// preserves the old per-characteristic callback shape.
func (c Characteristic) EnableNotifications(callback func(buf []byte)) error {
	log := c.Logger().With("characteristic", ShortUUID(c.uuid))

	ctx, cancel := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancel()
	if err := c.device.Client.SetNotify(ctx, c.device.PeripheralID, c.serviceUUID, c.uuid, true); err != nil {
		log.Debug("enabling notifications failed", "err", err)
		return err
	}
	log.Debug("notifications enabled")

	notifyRegistryFor(c.device.Client).subscribe(notifyKey{
		peripheralID:       c.device.PeripheralID,
		serviceUUID:        c.serviceUUID,
		characteristicUUID: c.uuid,
	}, func(buf []byte) {
		log.Debug("gatt notification", "bytes", hexBytes(buf))
		callback(buf)
	})
	return nil
}

// uuidFromBytes formats a 16-byte UUID the same way the old
// bluetooth.NewUUID([16]byte{...}) call sites in services.go/
// characteristics.go were laid out (big-endian, matching the standard
// 8-4-4-4-12 textual grouping directly - no byte-swapping).
func uuidFromBytes(b [16]byte) string {
	return fmt.Sprintf("%02X%02X%02X%02X-%02X%02X-%02X%02X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// uuid16 expands a Bluetooth SIG-assigned 16-bit UUID (e.g. 0x2A29 for
// Manufacturer Name String) to its full 128-bit form using the standard
// Bluetooth Base UUID.
func uuid16(n uint16) string {
	return fmt.Sprintf("0000%04X-0000-1000-8000-00805F9B34FB", n)
}
