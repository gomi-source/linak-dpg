package desk

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gomi-source/linak-dpg"
	"github.com/gomi-source/linak-dpg/subscription/controlerror"
	"github.com/gomi-source/linak-dpg/subscription/deskpanel"
	"github.com/gomi-source/linak-dpg/subscription/referenceoutput"
)

type Desk struct {
	ID   string
	Name string

	device dpg.Device
	gatt   *dpg.GATT
	roSub  referenceoutput.Subscribable
	dpSub  deskpanel.Subscribable

	// DeskPanel reads are synchronous: one at a time, each waiting for the
	// answer it belongs to. deskPanelMu enforces the one-at-a-time part
	// and slots hold the answers; see query.go for why both are needed.
	deskPanelMu sync.Mutex

	// lastTarget remembers the height last written to ReferenceInput and
	// when, across Move calls. The controller will not accept a different
	// height until it has been left alone for RetargetGap, and that is as
	// true between two moves as it is within one - so this outlives the
	// goroutine that wrote it.
	targetMu     sync.Mutex
	lastTarget   int
	haveTarget   bool
	lastTargetAt time.Time

	// lastPosition is the most recent height the desk reported. It goes
	// stale the moment a move ends, because a stationary desk reports
	// nothing - it is for diagnostics, not for decisions.
	lastPosition int
	havePosition bool

	// trackPosition registers the position recorder once. It is never
	// removed - the notify registry cannot drop a dispatcher anyway - so
	// after the first move the desk's position is followed continuously,
	// including while somebody moves it from its own panel.
	trackPosition sync.Once

	// reminderMu holds a reminder read and its write together. The desk
	// has no partial write, so changing one preset means rewriting all of
	// them, and two callers doing that at once would lose one another's
	// changes.
	reminderMu sync.Mutex
	slots      *deskPanelSlots

	// ceSub is the Control error characteristic, made on first use and
	// kept, like the others, for the life of the Desk. A failure to get it
	// is kept too: the characteristic is either there or it is not, and
	// asking again on every move could cost each one a discovery timeout.
	ceOnce sync.Once
	ceSub  controlerror.Subscribable
	ceErr  error

	chMove chan int
	moving atomic.Bool
}

// DefaultTimeout bounds how long a DeskPanel read waits for its answer
// when the caller's context carries no deadline of its own.
const DefaultTimeout = 5 * time.Second

func New(device dpg.Device, name string) *Desk {
	if device.IsZero() {
		return &Desk{}
	}

	desk := &Desk{
		device: device,
		gatt:   &dpg.GATT{},
		roSub:  referenceoutput.NewSubscription(device),
		slots:  newDeskPanelSlots(),
		chMove: make(chan int, 1),
		ID:     device.Address(), // Note: stable across reconnects on this Mac, but a different Mac sees a different ID for the same physical desk
		Name:   name,
	}

	return desk
}

// InitDeskPanelSubscription creates the DeskPanel subscription if it does
// not exist yet, and wires every response into the slot the matching read
// method waits on.
//
// The callbacks are registered once, here, and never removed. A
// subscription's dispatcher cannot be removed from the notify registry
// once started, and Subscription.AddCallback mutates its callback map
// without holding the lock its dispatch loop reads under - so registering
// per read would both leak and race.
func (d *Desk) InitDeskPanelSubscription() {
	if d.dpSub != nil {
		return // already created
	}
	if d.slots == nil {
		d.slots = newDeskPanelSlots()
	}

	d.dpSub = deskpanel.NewSubscription(d.device)

	d.dpSub.AddUserIDCallback(d.putUser)
	d.dpSub.AddBaseOffsetCallback(d.slots.baseOffset.put)
	d.dpSub.AddCapabilitiesCallback(d.slots.capabilities.put)
	d.dpSub.AddProductInfoCallback(d.slots.productInfo.put)
	d.dpSub.AddReminderSettingCallback(d.slots.reminder.put)
	d.dpSub.AddMemoryPositionCallback(d.slots.memory.put)
	d.dpSub.AddMemoryPositionUnsetCallback(func() {
		d.slots.memoryUnset.put(struct{}{})
	})
}

// putUser stores a reported user ID. The dispatch loop reuses its buffer,
// so the slot must hold a copy rather than alias it.
func (d *Desk) putUser(data []byte) {
	d.slots.user.put(append([]byte(nil), data...))
}

// DeskPanelSubscription returns the desk's DeskPanel subscription,
// creating it on first use.
//
// Callers that want their own DeskPanel callbacks should add them here
// rather than building a second subscription for the same desk:
// notifications are registered per (peripheral, characteristic) in a
// process-wide registry that has no removal path, so a second
// subscription on the same characteristic makes every DeskPanel response
// dispatch - and every callback fire - twice.
func (d *Desk) DeskPanelSubscription() deskpanel.Subscribable {
	d.InitDeskPanelSubscription()
	return d.dpSub
}

// Positions is the ReferenceOutput stream: the desk pushes its height and
// speed as it moves, unasked. That makes it the one genuinely
// subscription-shaped thing here, and the only part of the desk with a
// callback API - everything on the DeskPanel characteristic is
// request/response and has a method that returns a value instead.
//
// Move depends on this stream, so a caller that replaces it with
// SetPositions must keep it fed.
func (d *Desk) Positions() referenceoutput.Subscribable {
	return d.roSub
}

// SetPositions replaces the ReferenceOutput stream with one built
// elsewhere. Only one subscription per characteristic per peripheral
// should exist, so a caller that has already made its own passes it here
// rather than leaving two dispatchers running.
func (d *Desk) SetPositions(sub *referenceoutput.Subscription) {
	d.roSub = sub
}

// ControlErrors is the Control error characteristic, 99FA0003: the frames
// the controller sends when a move goes wrong - it ran into something, or
// was halted. Move watches it and ends a move on an error; see move.go.
//
// It is made on first use, and there is only ever one per Desk: a
// subscription's dispatcher can never be removed, so a second one on the
// same characteristic is a second dispatcher for the life of the client.
// Callers that want the frames too should add their callbacks here. The error is for a desk or a corebluetoothd helper without the
// characteristic, which is worth carrying on without.
func (d *Desk) ControlErrors() (controlerror.Subscribable, error) {
	d.ceOnce.Do(func() {
		if d.ceSub != nil {
			return
		}
		d.ceSub, d.ceErr = controlerror.NewSubscription(d.device)
	})
	return d.ceSub, d.ceErr
}

// Device getter
func (d *Desk) Device() dpg.Device {
	return d.device
}

// There are x parameters that can be requested from the DeskPanel Characteristic
/* from constnants.go:
DeskPanelCommandProductInfo
DeskPanelCommandGetCapabilities
DeskPanelCommandBaseOffset
DeskPanelCommandUserID
DeskPanelCommandReminderSetting
DeskPanelCommandMemoryPosition1
DeskPanelCommandMemoryPosition2
DeskPanelCommandMemoryPosition3
DeskPanelCommandMemoryPosition4
*/
func (d *Desk) requestBaseOffset() error {
	// Get DeskPanel Characteristic
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not request base offset: %v", err)
	}

	// Write request to Characteristic
	p := dpg.Envelope(dpg.DeskPanelCommandBaseOffset)
	if _, err := c.Write(p); err != nil {
		return fmt.Errorf("error when writing base offset request: %v", err)
	}

	return nil
}

// Set base offset as height in 1/10mm from the floor
func (d *Desk) WriteBaseOffset(mmx10 int) error {
	// Get DeskPanel Characteristic
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not update base offset: %v", err)
	}

	// Pack and write new value to Characteristic
	bytes := dpg.NewHeight(mmx10).Bytes() // height from floor in mm * 10
	payload := dpg.EnvelopeWithPayload(dpg.DeskPanelCommandBaseOffset, bytes[:])
	_, err = c.Write(payload) // Nota bene userid has to have their owner bit set first
	if err != nil {
		return fmt.Errorf("error when writing base offset: %v", err)
	}

	return nil
}

// requestReminder sends the question only. Reminder waits for the answer.
func (d *Desk) requestReminder() error {
	// Get DeskPanel Characteristic
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not request reminder settings: %v", err)
	}

	// Write request to Characteristic
	p := dpg.Envelope(dpg.DeskPanelCommandReminderSetting)
	if _, err := c.Write(p); err != nil {
		return fmt.Errorf("error when writing reminder settings request: %v", err)
	}

	return nil
}

// WriteReminder replaces every reminder setting on the desk at once: which
// preset is active, or that reminders are off, and the intervals of all
// three presets. One write carries all of it - the controller offers no
// way to change part - so a zero Reminder clears the lot.
//
// Most callers want WriteReminderPreset or WriteActiveReminder instead,
// which read the current settings and change only the part named. This is
// for a caller that already holds a whole Reminder and means to write all
// of it.
//
// Like every write, it is silently ignored unless TakeOwnership has succeeded.
//
// Confirmed against a DPG1M by example/frames -write-reminder: preset 1
// written as 23 sitting / 61 standing came back as exactly that, with the
// other two presets untouched. The desk answers with the same two-byte
// acknowledgement it sends for any write, so it confirms only that
// something was accepted - read the settings back to know what.
func (d *Desk) WriteReminder(r dpg.Reminder) error {
	if !r.IsValid() {
		return fmt.Errorf("could not write reminder settings: invalid Reminder %+v", r)
	}

	// Get DeskPanel Characteristic
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not write reminder settings: %v", err)
	}

	// Pack and write new value to Characteristic. The trailing counter
	// bytes go out as zeros; the desk keeps its own.
	bytes := r.Bytes()
	payload := dpg.EnvelopeWithPayload(dpg.DeskPanelCommandReminderSetting, bytes[:])
	if _, err := c.Write(payload); err != nil {
		return fmt.Errorf("error when writing reminder settings: %v", err)
	}

	return nil
}

// WriteReminderPreset changes one preset's intervals, 1 to 3, leaving the
// other two and the active selection as they are.
//
// The controller has no partial write - one write carries every setting -
// so this reads the current settings, alters the preset named, and writes
// the result back. That is a quirk of the hardware rather than something a
// caller should have to work around, which is why this exists.
//
// Note that changing the active preset's intervals takes effect
// immediately; changing another preset's does not, until it is selected.
func (d *Desk) WriteReminderPreset(ctx context.Context, preset int, intervals dpg.ReminderIntervals) error {
	if preset < 1 || preset > 3 {
		return fmt.Errorf("could not write reminder preset: %d is not between 1 and 3", preset)
	}

	return d.updateReminder(ctx, fmt.Sprintf("preset %d", preset), func(r *dpg.Reminder) {
		applyPreset(r, preset, intervals)
	})
}

// applyPreset sets one preset's intervals and touches nothing else. Split
// out so the "only that preset changes" rule can be tested without a desk.
func applyPreset(r *dpg.Reminder, preset int, intervals dpg.ReminderIntervals) {
	switch preset {
	case 1:
		r.Option1 = intervals
	case 2:
		r.Option2 = intervals
	case 3:
		r.Option3 = intervals
	}
}

// WriteActiveReminder selects which preset the desk reminds on, or turns
// reminders off with dpg.ReminderOff, leaving all three presets' intervals
// as they are.
//
// Like WriteReminderPreset, this reads before writing because the
// controller has no partial write.
func (d *Desk) WriteActiveReminder(ctx context.Context, option dpg.ReminderOption) error {
	switch option {
	case dpg.ReminderOff, dpg.ReminderOption1, dpg.ReminderOption2, dpg.ReminderOption3:
	default:
		return fmt.Errorf("could not write active reminder: %d is not a reminder option the desk knows", option)
	}

	return d.updateReminder(ctx, "active option", func(r *dpg.Reminder) {
		r.ActiveOption = option
	})
}

// updateReminder is the read-modify-write the controller forces on anyone
// changing part of the reminder settings.
//
// reminderMu holds the pair together. Without it two callers changing
// different presets could both read the old settings and the second write
// would quietly undo the first - the classic lost update, and an easy one
// to hit here because a whole write is the only kind there is.
func (d *Desk) updateReminder(ctx context.Context, what string, change func(*dpg.Reminder)) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	d.reminderMu.Lock()
	defer d.reminderMu.Unlock()

	current, err := d.Reminder(ctx)
	if err != nil {
		return fmt.Errorf("could not read reminder settings before changing %s: %w", what, err)
	}

	change(&current)

	if err := d.WriteReminder(current); err != nil {
		return fmt.Errorf("could not write %s: %w", what, err)
	}
	return nil
}

// requestUser sends the question only. User waits for the answer.
func (d *Desk) requestUser() error {
	// Get DeskPanel Characteristic
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not request user: %v", err)
	}

	// Write request to Characteristic
	p := dpg.Envelope(dpg.DeskPanelCommandUserID)
	if _, err := c.Write(p); err != nil {
		return fmt.Errorf("error when writing user request: %v", err)
	}

	return nil
}

// User reads the user ID currently stored on the desk.
//
// The first byte is the owner flag; see TakeOwnership, which is what most
// callers want.
func (d *Desk) User(ctx context.Context) (*dpg.User, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	data, err := query(ctx, d, d.slots.user, "user ID", d.requestUser)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("desk reported an empty user ID")
	}
	return dpg.NewUser(data), nil
}

// BaseOffset reads the stored distance from the floor to the desk's lowest
// position, in tenths of a millimetre. It is a value the desk stores
// rather than measures; see WriteBaseOffset.
func (d *Desk) BaseOffset(ctx context.Context) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	return query(ctx, d, d.slots.baseOffset, "base offset", d.requestBaseOffset)
}

// Capabilities reads what this controller supports, including how many
// memory positions it actually has - which is worth knowing before writing
// one, since a desk answers for a position it does not have exactly as it
// answers for one that was never set.
func (d *Desk) Capabilities(ctx context.Context) (dpg.Capabilities, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	return query(ctx, d, d.slots.capabilities, "capabilities",
		func() error { return d.request(dpg.DeskPanelCommandGetCapabilities, "capabilities") })
}

// ProductInfo reads the controller's model and firmware details.
func (d *Desk) ProductInfo(ctx context.Context) (dpg.ProductInfo, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	return query(ctx, d, d.slots.productInfo, "product info",
		func() error { return d.request(dpg.DeskPanelCommandProductInfo, "product info") })
}

// Reminder reads the stand/sit reminder settings: which preset is active,
// or that reminders are off, and the intervals of all three presets.
func (d *Desk) Reminder(ctx context.Context) (dpg.Reminder, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	return query(ctx, d, d.slots.reminder, "reminder settings", d.requestReminder)
}

// MemoryPosition reads one of the panel's memory buttons, 1 to 4.
//
// A position with no height stored returns a MemoryPosition whose
// Extension is nil and a nil error: empty is an answer, not a failure.
// That covers a position the desk does not have as well as one that was
// never set or has been cleared - the desk answers identically for all
// three, so ask Capabilities how many this controller has.
//
// The reply says nothing about which position it describes, which is why
// this waits for it rather than handing the caller a callback: only one
// question is outstanding at a time, so the answer belongs to this
// request.
func (d *Desk) MemoryPosition(ctx context.Context, position int) (dpg.MemoryPosition, error) {
	m := dpg.MemoryPosition{Position: position}
	if !m.IsValid() {
		return m, fmt.Errorf("could not read memory position: %d is not between 1 and 4", position)
	}
	if d.device.IsZero() {
		return m, fmt.Errorf("could not read memory position: desk has no device")
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	d.InitDeskPanelSubscription()
	d.deskPanelMu.Lock()
	defer d.deskPanelMu.Unlock()

	// Either reply answers this request, so both slots are cleared first
	// and both are waited on.
	d.slots.memory.drain()
	d.slots.memoryUnset.drain()

	if err := d.request(m.Command(), "memory position"); err != nil {
		return m, err
	}

	select {
	case extension := <-d.slots.memory.ch:
		m.Extension = &extension
		return m, nil
	case <-d.slots.memoryUnset.ch:
		return m, nil // Extension stays nil: nothing stored
	case <-ctx.Done():
		return m, fmt.Errorf("gave up waiting for the desk to report memory position %d: %w", position, ctx.Err())
	}
}

// request sends a bare read request for one DeskPanel parameter.
func (d *Desk) request(cmd dpg.DeskPanelCommand, what string) error {
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not request %s: %v", what, err)
	}
	if _, err := c.Write(dpg.Envelope(cmd)); err != nil {
		return fmt.Errorf("error when writing %s request: %v", what, err)
	}
	return nil
}

// TakeOwnership makes sure the desk will accept writes from this user.
//
// It is a verb rather than a SetX because it talks to the desk: two round
// trips at worst. Set* on a Desk is reserved for plain assignment.
//
// A DPG controller silently ignores writes - ReferenceInput (which
// Move depends on), WriteBaseOffset, WriteMemoryPosition - unless the user
// ID stored on it has its owner bit set. The desk clears that bit by
// itself in some situations, so this is worth calling after every
// (re)connect rather than once at setup; it is idempotent and costs one
// round trip when the bit is already set.
//
// It reports whether the bit had to be set: false with a nil error means
// the desk already considered us the owner. When the bit is written, the
// user ID is read back to confirm the desk accepted it, so a nil error
// really does mean writes will now work.
func (d *Desk) TakeOwnership(ctx context.Context) (bool, error) {
	user, err := d.User(ctx)
	if err != nil {
		return false, err
	}
	if user.Owner() {
		return false, nil
	}

	user.SetOwner(true)
	if err := d.WriteUser(*user); err != nil {
		return false, err
	}

	confirmed, err := d.User(ctx)
	if err != nil {
		return false, fmt.Errorf("owner bit written but could not be confirmed: %v", err)
	}
	if !confirmed.Owner() {
		return false, fmt.Errorf("desk did not accept the owner bit for user %v", confirmed.ID)
	}

	return true, nil
}

func (d *Desk) WriteUser(user dpg.User) error { // userId []byte) error {
	// Get DeskPanel Characteristic
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not write user ID: %v", err)
	}

	// Pack and write new value to Characteristic
	bytes := user.Data //  []byte { 1, 51, 251, 186, 150, 20, 68, 21, 130, 7, 249, 110, 204, 212, 167, 117, 182 }
	payload := dpg.EnvelopeWithPayload(dpg.DeskPanelCommandUserID, bytes)
	_, err = c.Write(payload)
	if err != nil {
		return fmt.Errorf("error when writing user ID: %v", err)
	}

	return nil
}

// Write an extension (from base height in tenths of mm) to a favourite position
// Read desk capabilities in order to ensure the memory position is available
func (d *Desk) WriteMemoryPosition(m dpg.MemoryPosition) error {
	if !m.IsValid() {
		return fmt.Errorf("could not write memory position: invalid MemoryPosition %v", m)
	}

	// Get DeskPanel Characteristic
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel)
	if err != nil {
		return fmt.Errorf("could not update base offset: %v", err)
	}

	payload := m.Envelope()
	_, err = c.Write(payload)
	if err != nil {
		return fmt.Errorf("error when writing %v to memory position %v: %v", payload, m.Position, err)
	}

	return nil
}

func (d *Desk) UnsetMemoryPosition(position int) error {
	m := dpg.MemoryPosition{Position: position}
	return d.WriteMemoryPosition(m)
}

// WakeUp tells the controller to wake, and Stop tells it to halt. Both go
// to the Control characteristic.
//
// Beyond their obvious uses, a wake-up is worth trying right after
// connecting: a DPG controller drops the link by itself within seconds of
// a connection that only reads, and the wake-up may be what a panel sends
// to keep a session alive. That is a hypothesis, not established
// behaviour - see the README.
func (d *Desk) WakeUp() error { return d.control(dpg.ControlCommandWakeUp) }

// Stop halts any movement in progress.
func (d *Desk) Stop() error { return d.control(dpg.ControlCommandStop) }

// Moving reports whether a move started by Move is still running: from
// the call that starts it until the move has arrived, stopped short, or
// given up. It is this package's verdict, not the desk's - a desk moved
// from its own panel is not "moving" here - and it is what to wait on to
// know that a move is over, since a speed 0 in the position stream can
// also be a retarget pausing on the way.
func (d *Desk) Moving() bool { return d.moving.Load() }

// control writes one command to the Control characteristic. It writes
// without response: these commands are not answered, and a
// write-with-response on a characteristic that does not support it would
// stall until the RPC timeout.
func (d *Desk) control(cmd dpg.ControlCommand) error {
	c, err := d.gatt.GetCharacteristic(d.device, dpg.ServiceUUIDControl, dpg.CharacteristicUUIDControlCommand)
	if err != nil {
		return fmt.Errorf("could not get control characteristic: %v", err)
	}

	if _, err := c.WriteWithoutResponse(cmd.Bytes()); err != nil {
		return fmt.Errorf("error when writing control command %v: %v", byte(cmd), err)
	}

	return nil
}
