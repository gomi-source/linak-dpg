# linak-dpg

A Go implementation of the LINAK **D**esk **P**anel **G**ATT protocol —
the BLE service a LINAK DPG desk controller exposes for reading its
position and driving it up and down.

It speaks GATT and nothing else. Scanning and connecting happen wherever
the `*ble.Client` comes from; hand this package a connected peripheral and
it gives you a `Desk`.

```
your program  -->  linak-dpg  -->  corebluetooth-go  -->  CoreBluetooth  -->  DPG controller
                  (this repo)
```

The BLE transport is [`corebluetooth-go`](https://github.com/gomi-source/corebluetooth-go),
which drives macOS CoreBluetooth from Go without cgo by way of a small
Swift helper process. That makes this package macOS-only in practice,
though nothing in it is platform-specific — swapping the transport would
mean replacing `Device`, `GATT` and `notify.go`.

Developed against a **DPG1M**. Other LINAK controllers advertise the same
services and should work, but the comments mark the places where a value
was guessed from one desk's behaviour rather than from documentation.

> **Unofficial.** This project is not affiliated with, endorsed by, or
> supported by LINAK A/S. "LINAK" and "DPG" are the manufacturer's names,
> used here only to say which hardware this speaks to. The protocol was
> worked out by observing one desk controller, not from any specification
> LINAK published, so nothing here is authoritative and a firmware revision
> could invalidate any of it. Use it at your own risk: it drives a motor
> attached to a heavy piece of furniture.

## Installing

```sh
go get github.com/gomi-source/linak-dpg
```

While the three repos are developed together, `go.mod` resolves
`corebluetooth-go` through a `replace` directive, so they need to sit
side by side in the same parent directory.

## Quick start

```go
ctx := context.Background()

client, err := ble.Start(ctx, ble.Options{Stderr: os.Stderr})
if err != nil {
    log.Fatal(err)
}
defer client.Close()

// A DPG controller does not advertise its services, so scan unfiltered
// and match on the name. See "Finding a desk" below.
if err := client.StartScan(ctx, ble.ScanOptions{}); err != nil {
    log.Fatal(err)
}
var peripheralID string
for p := range client.Discoveries() {
    if p.Name != nil && *p.Name == "DESK 8352" {
        peripheralID = p.PeripheralID
        break
    }
}
_ = client.StopScan(ctx)

if err := client.Connect(ctx, peripheralID, 30*time.Second); err != nil {
    log.Fatal(err)
}
defer func() { _ = client.Disconnect(ctx, peripheralID) }()

d := desk.New(dpg.NewDevice(client, peripheralID), "office")

// The controller ignores every write unless it considers us its owner.
if _, err := d.TakeOwnership(ctx); err != nil {
    log.Fatal(err)
}

d.Positions().AddReferenceOutputCallback(func(extension, speed int) {
    log.Printf("height %d, speed %d", extension, speed)
})

// Move returns as soon as movement is under way; a background goroutine
// keeps writing the target until the desk reports speed 0.
if err := d.Move(7350); err != nil {
    log.Fatal(err)
}
time.Sleep(15 * time.Second)
```

## Units and reference frames

Every height in this package is an integer in **tenths of a millimetre**,
which is what the controller itself uses. Two different frames are in
play, and mixing them up is the easiest mistake to make here:

| Term | Means | Where |
|---|---|---|
| **extension** | Height above the desk's own lowest position | `Move`, ReferenceOutput notifications, memory positions |
| **base offset** | Distance from the floor to that lowest position | `BaseOffset`, `WriteBaseOffset` |

Height above the floor is `base offset + extension`. Nothing in this
package computes it for you.

The base offset is a value *stored on the controller* — the desk does not
measure it. It is there so a panel can display a meaningful number, and
writing it changes the display, not the desk.

## Ownership

**A DPG controller silently ignores writes from a user it does not
consider its owner.** No error, no rejection — moves simply do nothing.
The controller stores a user ID whose first byte is an owner flag, and it
clears that flag by itself in some situations, so this is worth asserting
after every connect rather than once at setup:

```go
changed, err := d.TakeOwnership(ctx)
```

`TakeOwnership` reads the stored user ID, and if the owner bit is already set
returns `(false, nil)` having done nothing. Otherwise it sets the bit,
writes it back, reads it again to confirm the controller accepted it, and
returns `(true, nil)`. A nil error therefore means writes will now work —
without the read-back it would mean very little, since a rejected write is
indistinguishable from a successful one.

If moves are being accepted by your code and ignored by the desk, this is
almost always why.

## Finding a desk

A DPG controller does **not** put its services in its advertisement
packets. The control service (`99FA0001-…`) only becomes visible after
connecting and discovering, and CoreBluetooth matches scan filters against
the advertisement alone — so scanning with `ServiceUUIDs: []string{ServiceUUIDControl}`
finds nothing at all. Scan unfiltered and identify the desk by its
advertised name or by its CoreBluetooth peripheral ID.

Note that a peripheral ID is stable across reconnects on one Mac but
differs between Macs for the same physical desk.

## Packages

### `dpg` — protocol primitives

Service and characteristic UUIDs, the DeskPanel command envelope, and the
value types.

| Service | UUID | Holds |
|---|---|---|
| Control | `99FA0001-…` | Command and error characteristics |
| DeskPanel | `99FA0010-…` | The settings characteristic: capabilities, base offset, product info, memory positions, reminders, user ID |
| ReferenceOutput | `99FA0020-…` | Position notifications |
| ReferenceInput | `99FA0030-…` | Where a move target is written |
| DeviceInformation | `180A` | Manufacturer, model |

`Device` is a connected peripheral: a `*ble.Client` plus a peripheral ID.
`GATT` discovers services and characteristics on one and caches them per
peripheral; call `GATT.Disconnect(device)` to drop that cache, which is
necessary on every reconnect because CoreBluetooth invalidates service
and characteristic objects across a disconnect.

The DeskPanel characteristic takes a small envelope. `Envelope(cmd)`
builds a read request; `EnvelopeWithPayload(cmd, payload)` builds a write.

Commands for which `NeedsExtensionByte` reports true carry one further
byte between the envelope and the payload, which the desk reads as "a
value follows" (1) or "there is no value" (0) — the same byte it returns
as `data[2]` in the response. That byte is not a free choice: it follows
from what is being written. Base offset only ever writes a value, so
`EnvelopeWithPayload` supplies 1 and that is the end of it. Clearing a
memory position is the sole case needing 0, and it packs its own envelope:

```go
c.Write(m.Envelope())  // stores m's height, or clears the slot if Extension is nil
```

The payload itself must *not* repeat that byte: doing so shifts everything
along by one, and the desk stores the extension byte plus the first height
byte as the height.

**The byte belongs to specific commands, not to writes in general.**
Reminder settings were written back unchanged both with and without one
(`example/frames -probe-reminder`): without, they round-tripped exactly;
with, the desk stored `01` followed by the first six settings bytes and
dropped the seventh — the same one-byte shift. User ID likewise takes no
extension byte and writes fine. So `NeedsExtensionByte`'s list is the
authority, and adding the byte where it is not wanted corrupts the write
rather than being ignored. Whether "carries a height" is the underlying
rule, or those five commands simply happen to be the ones that parse it,
is still open — every command known to take it writes a height, and every
command known not to does not.

### `dpg/desk` — the desk

`desk.New(device, name)` wraps a connected `Device`.

| | |
|---|---|
| `Move(mmx10)` | Move to an extension. Returns immediately; see below. |
| `BaseOffset(ctx)` / `WriteBaseOffset(mmx10)` | The stored floor offset |
| `User(ctx)` / `WriteUser(u)` / `TakeOwnership(ctx)` | The stored user ID and its owner bit |
| `MemoryPosition(ctx, n)` / `WriteMemoryPosition(m)` / `UnsetMemoryPosition(n)` | The panel's 1–4 memory buttons |
| `Reminder(ctx)` / `WriteReminder(r)` | The stand/sit reminder presets; one write carries all three |
| `Capabilities(ctx)` / `ProductInfo(ctx)` | What the controller is and supports |
| `Positions()` | The ReferenceOutput stream |
| `DeskPanelSubscription()` | Escape hatch; see below |

**Reads are synchronous.** The DeskPanel characteristic is
request/response wearing a subscription's clothes — you ask for a
parameter, the desk answers once, and it arrives as a notification only
because that is how GATT delivers it. So `BaseOffset(ctx)` sends the
request and returns the value.

Two things make that the right shape rather than merely a convenient one.

A reply carries nothing identifying the request it answers. Different
parameters answer under different response codes, so those are never
confused — but two reads of the same parameter in flight are
indistinguishable, and a memory position reply says nothing about which
position it describes. `Desk` therefore allows one DeskPanel query at a
time. That serialisation is load-bearing: it is what makes an answer
attributable at all.

And the desk never speaks first. Storing a favourite from the panel — the
only change the panel itself can make — pushes no frame, so nothing
arrives that is not an answer. Verified with `example/frames -listen`.
`DeskPanelSubscription()` remains for anyone who wants the raw stream, but
nothing in normal use needs it.

`MemoryPosition(ctx, n)` returns a `MemoryPosition` with a nil `Extension`
and a nil error when nothing is stored: empty is an answer, not a failure.
It joins up the two response codes an empty position can answer with,
which a callback API leaves to the caller.

A `ctx` without a deadline gets `DefaultTimeout` (5s), so a dropped link
cannot hang a read forever.

`Positions()` is the one genuine subscription. ReferenceOutput is a
*stream*: the desk pushes height and speed as it moves, unasked, and
`Move` depends on it. That difference in kind — stream versus
request/response — is why it keeps a callback API while everything on
DeskPanel does not.

`Move` returns as soon as the first target has been written. A background
goroutine then rewrites it until the desk arrives, and gives up after 5
seconds without progress.

**A halt is not an arrival, and it is not pushed through.** The desk
reports speed 0 both when it has reached the target and when it has
stopped short — and one reason it stops short is that it has hit
something. Arrival is *speed 0 and within 2 mm of the target*. A halt
anywhere else, once the desk has been moving, **ends the move**: the
target is not written again. The controller's own safety stop is the only
protection whatever it met has, and re-commanding would drive the desk
back into it. The move logs where it stopped and how far short; whether to
ask again belongs to the caller, who may know the way is clear.

Before the desk has moved at all, the target is offered for a short grace
period — it takes a moment to pick the first one up — and then given up on
with a warning. That window is deliberately brief for the same reason.

Calling `Move` again while moving retargets rather than starting a second
move — but a retarget is a new move, not an assignment. **The desk only
accepts a different height once it has come to rest.** Writing one sooner
halts it instead of redirecting it.

Rewriting the *same* height is what sustains movement, and stopping the
writes is what stops the desk — so a retarget goes quiet, waits for the
stream to report speed 0, and only then writes the new height. The silence
is the braking. A reversal additionally sends `Stop`, bringing it to rest
deliberately rather than by coasting.

`RetargetGap` (800 ms) is the floor under that wait, not the wait itself:
the retarget resumes once the desk is at rest *and* the gap has elapsed,
whichever is later. The fixed figure came from trial before the speed
stream was being watched, and keeping it means this is never quicker off
the mark than what was known to work; waiting for rest is what covers a
desk still decelerating after 800 ms, which a fixed pause could not. It is
a package variable, so another controller can be given another figure.

Anything the desk reports during that gap — including speed 0 — is the old
move ending, not a fault. The halt in the middle of a retarget is ours;
only a halt during travel is the desk's own.

**A burst of targets costs one gap, not one each.** Targets arriving while
the desk is quiet supersede the one that started the gap, so `1500, 1700,
1900, 2100` sent in quick succession interrupts the desk once and then
runs to 2100 — rather than pausing 800 ms per nudge. The direction check
is made after the gap against the final target, so a burst that ends up
pointing the other way is still stopped properly first.

Position reports go into a one-slot holder, never a queue. They arrive on
their own goroutine each, so a blocking handoff leaves a backlog of
readings from seconds ago — and a speed 0 recorded *before* a retarget
then surfaces *after* it and reads as the new move finishing. That is a
real failure, not a theoretical one: a short first move that completes
while the retarget is pending ends the whole sequence one write into the
second move.

### `dpg/subscription` — notification plumbing

A characteristic's notifications, demultiplexed by message type.
`AddCallback(msgType, fn)` returns a function that removes the callback;
notifications are enabled lazily on the first registration.

Registering and removing are safe from any goroutine, including from
inside a handler. The callback map is only touched under the
subscription's mutex, and the dispatch loop copies the handlers for a
frame before calling them rather than ranging over the map while it does
— which matters more than a data race would: writing a map that is being
ranged over is a fatal runtime error, so the old unlocked version could
take the process down rather than merely misbehave.

A memory position has two possible replies, under two different response
codes, so it needs two callbacks. One that holds a height answers with
code 7, nine bytes — `01 07 01 <2B extension> <4B counter>` — and reaches
`AddMemoryPositionCallback`. One with no height to report answers with
code 5, seven bytes — `01 05 00 FF FF FF FF`, echoing the same `00`
extension byte and `FFFFFFFF` counter that clear one — and
reaches `AddMemoryPositionUnsetCallback`. Register only the first and such
a position produces no notification at all.

Code 5 means "no height", not "no such position" — confirmed by clearing
position 2 on a desk whose capabilities report two positions, and getting
code 5 back from a position that certainly exists.

Its counter says which kind of empty. A cleared position carries a real
timestamp; one that never held a value carries `FFFFFFFF`. That second
case is indistinguishable from a position the controller does not have,
since positions 3 and 4 on a two-position desk answer identically.

Whether code 5 is specific to memory positions or a general "nothing
stored" reply is still unknown — nothing else has been seen to use it.

The trailing four bytes of both replies are a counter the desk maintains
itself, ticking at roughly 1 Hz and stamped whenever a position is
written. Writes can leave it zero; the desk substitutes its own.

**Reminder settings carry the same counter**, with the same behaviour:
four little-endian bytes after the seven settings bytes, jumping to the
present on every accepted write and advancing about once a second
thereafter. Seeing it on a second, unrelated command is what makes
"desk-maintained timestamp of the last write" a description rather than a
guess. Anything comparing two reads of either value has to exclude it.

The seven bytes before it are the active option followed by all three
presets, each as sitting minutes then standing minutes. **Sitting comes
first**, which the field order in `ReminderIntervals` does not suggest:
with preset 1 set from LINAK's app to stand 60 / sit 20, the desk reports
that preset as `14 3C` (20, 60). The app shows the pair the other way
round, standing first, which is the likely origin of the field order.

Both intervals of every preset are editable and bear no fixed relationship
to each other, so `55/5, 50/10, 45/15` are shipped defaults rather than
anything structural; editing one preset leaves the other two alone. All
four values of the first byte
(`ReminderOff` and the three `ReminderOption`s) were read back while
switching the selection on the panel, and the pairs do not move with it —
they are the presets themselves, not the active one. That makes reminders
one of the few parts of this protocol that is observed end to end rather
than inferred.

`AddWriteCallback` catches the acknowledgement the controller sends for a
write. It is the same frame whatever was written, so it confirms that
*something* was accepted and nothing more — see the note under "Known
rough edges". To learn whether a particular write took effect, read the
value back.

Note that acknowledgements are delivered to the write callback *in
addition to* whichever typed callback matches their second byte, not
instead of it. The typed parsers discard them on length.

`dpg/subscription/deskpanel`, `.../referenceoutput` and
`.../controlerror` wrap that with parsing, so you get
`AddBaseOffsetCallback(func(int))` rather than a raw byte slice.

**A subscription's dispatcher cannot be removed once started.**
`AddCallback` returns a working remove function, and `start()` is
idempotent, so one `Subscription` enables notifications exactly once — but
the handler it registers in the process-wide notify registry stays there
for the life of the client. Remove every callback and the dispatcher lives
on, decoding frames for nobody.

Two subscriptions on one characteristic are therefore wasteful rather than
wrong: each feeds its own callbacks, so nothing fires twice. The hazard is
*rebuilding* them — make a new subscription on reconnect, re-register your
callback on it, and the old dispatcher and old callback are both still
live, so every reading arrives twice. Create them once and re-arm the
characteristic through the BLE client instead.

`Desk.Positions()` and `Desk.DeskPanelSubscription()` exist so callers can
share the desk's own rather than building a second.

## Reconnecting

CoreBluetooth invalidates a peripheral's service and characteristic
objects when it disconnects, and this package caches its own handles per
peripheral ID. Both have to be rebuilt on every reconnect:

```go
gatt.Disconnect(device)                                  // drop the cache
gatt.GetCharacteristic(device, service, characteristic)  // rediscover
```

Do **not** re-run `EnableNotifications` (or rebuild the subscription
objects) afterwards — that would register a second callback in the notify
registry, which cannot be undone, and every reading would then be
delivered twice. Re-arm the characteristic through the BLE client
directly instead:

```go
client.SetNotify(ctx, peripheralID, serviceUUID, characteristicUUID, true)
```

## Logging

Nothing is logged unless you ask for it, and nothing is ever written to
stdout. Give a `Device` an `*slog.Logger` and everything built from it —
its characteristics, its subscriptions, the `desk` package — logs through
that logger:

```go
device := dpg.NewDevice(client, peripheralID).WithLogger(log)
d := desk.New(device, "office")
```

`Device` is a value type and `WithLogger` returns a copy, so use the
returned value rather than the original.

At debug level this logs every GATT write and every notification with its
bytes, which is what you want when a desk accepts a command and then does
nothing:

```
level=DEBUG msg="gatt write" characteristic=99FA0011 bytes="7F 89 80 01 C0 12 00 00 00 00" with_response=true
level=DEBUG msg="gatt notification" characteristic=99FA0011 bytes="01 07 01 C0 12 5D 17 F6 05"
```

Warnings are used for the one case that is silent on the wire: a `Move`
that times out with no movement reported, which almost always means
`TakeOwnership` has not succeeded.

## Probing the protocol

`example/frames` records every raw notification the DeskPanel
characteristic emits, labelled with the request that provoked it. It
bypasses the subscription machinery entirely — that routes by message type
and drops short frames, which is the behaviour under investigation — and
reads notifications straight off the ble client.

```sh
make -C ../corebluetooth-go helper   # once, if the helper is not built yet

go run ./example/frames -name "DESK 8352"            # reads only, safe
go run ./example/frames -name "DESK 8352" -writes    # also writes values back unchanged
go run ./example/frames -name "DESK 8352" -unset-memory 4   # destructive, restores
go run ./example/frames -name "DESK 8352" -probe-reminder   # destructive, restores
go run ./example/frames -name "DESK 8352" -write-reminder   # destructive, restores
go run ./example/frames -name "DESK 8352" -listen 2m  # catch frames the desk sends unasked
go run ./example/frames -name "DESK 8352" -hold 10m  # measure how long the link lasts
```

Run it from the repo root: it looks for `corebluetoothd` in the usual
sibling locations, because `ble.Start`'s own lookup cannot help under
`go run` — that checks next to the running executable, which is a
temporary build directory, and then `$PATH`, where an entry has to name
the directory holding the executable itself
(`.../corebluetoothd.app/Contents/MacOS`) rather than the `.app` bundle or
its parent. `-helper` overrides, and wants that same inner path.

It requests each DeskPanel parameter in turn, then optionally writes back
exactly what it read (leaving the desk's state unchanged) to provoke
acknowledgements. `-unset-memory N` clears memory position N and then asks
for it again — the only way to see what an unset position reports, and it
really does clear the button.

`-probe-reminder` is what settled that. It writes the reminder settings
back unchanged twice — once in the form `EnvelopeWithPayload` produces and
once with an extension byte forced in — reading back after each, and
restores unconditionally. Reminder settings were the right subject because
nothing depends on them: a misparsed write costs only the settings the
probe puts back. They are the natural subject again for checking
`WriteReminder`, which is the one write in this package no desk has been
seen to accept.

It compares the seven settings bytes only. The four that follow them are a
counter the desk maintains for itself, which moves on every write, so
including them would make every run look like a failure.

`-write-reminder` is the counterpart in the write direction, and the one
mode that goes through the typed API rather than bytes the tool builds
itself. It sets preset 1 through `desk.WriteReminder` to a deliberately
lopsided 23 sitting / 61 standing, reads back what the desk stored, and
restores by the raw path. It asserts the owner bit first, since a write
without it is ignored in a way indistinguishable from a rejected one, and
it decodes the reply itself rather than through `deskpanel`'s parser, so
the check does not lean on the code it is checking. The three outcomes it
reports are: the desk stored sitting first (`WriteReminder` confirmed);
the desk stored standing first (`Reminder.Bytes` has the pair backwards);
or nothing changed (the write never took effect, which says nothing about
the packing). Against a DPG1M it reports the first: `38 17 3D ...` came
back for a preset written as 23 sitting / 61 standing.

`-listen` asks the one question the rest of the tool cannot: does the desk
ever speak first? Everything else here is request/response, so it would
miss a frame nobody asked for. Storing a favourite is the only change the
panel itself can make, which makes it the case to try. The answer decides
whether a request/response wrapper over DeskPanel is the whole story: an
unprompted store arrives under the same response code as the answer to a
memory read, and says no more about which position it describes, so a
pending read could mistake it for its own reply.

Three summaries at the end answer the open questions above: which
acknowledgement frames appeared and after which writes (if they are all
identical, acks cannot be attributed and serialising writes is the only
option); the minimum length seen per response type, which is how high each
typed parser's length guard could be raised; and the full frame log per
request.

## Known rough edges

This is a prototype, and these are known:

- **The connection drops unpredictably, and nothing has been found that
  provokes or prevents it entirely.**

  There appear to be two modes. In the bad one the controller drops the
  link over and over, sometimes for minutes, sometimes for hours; in
  the ordinary one it behaves like any BLE peripheral, dropping occasionally
  for no visible reason. A Control wake-up seems to end the bad mode, and
  the effect persists across later connections rather than being a per-session
  handshake — which is also why runs before and after one cannot be compared
  as independent samples. LINAK's own iPhone app sends a wake-up and a stop
  on startup, which is the strongest evidence there is that this is expected
  of a client. `Desk.WakeUp` and `Desk.Stop` exist for it, and `mqtt-linak`
  sends both on every connect.

  Beyond the wake-up, treat the remaining drops as ordinary link loss:
  CoreBluetooth exposes no control over the connection interval or
  supervision timeout, so there is nothing to tune on macOS.

  What matters is recovering well. After a drop, CoreBluetooth nils out
  the peripheral's services, so every subsequent call fails with
  `service not found ... call peripheral.discoverServices first` — a
  misleading message for a dropped link, and the reason a drop is easy to
  misdiagnose. Reconnect, rediscover, re-arm notifications, and the
  requests succeed again.

- **The notify registry cannot unsubscribe.** `notify.go` only appends.
  Everything above about reusing subscriptions follows from this.
- **The typed parsers' `len(data)` guards are what keep acknowledgements
  out.** The dispatch loop's two branches are additive, not exclusive: a
  frame goes to the callback registered for `data[1]`, and then, if it is
  two bytes long, also to the write-acknowledgement callback. So an ack
  whose second byte happens to match a response code — `[1 7]` against
  `DeskPanelResponseMemoryPosition` — arrives at that typed callback as
  well, and only the length guard stops it being parsed as a real
  response. Why each threshold sits where it does (4 for memory position
  rather than the full 9) is not recorded; what matters is that they are
  above every acknowledgement length seen so far. `example/frames` reports
  the minimum length per response type, which is how high each guard could
  safely go. Observed against a DPG1M: capabilities 4, base offset 5,
  product info 8, memory position 9, reminder 13, user id 19 — so
  `AddProductInfoCallback`'s guard of `< 6` is the one that is actually
  too low, since it reads offset 6 and so needs at least 7.

  They are not bounds checks, though: a 4-byte frame would make
  `AddMemoryPositionCallback` read past the end, and a 6-byte one
  `AddProductInfoCallback`. Neither length occurs in the protocol as
  observed, so that only matters if another controller answers
  differently.
- **`WriteReminder` rewrites every preset at once.** One write carries the
  active option *and* all three presets, so there is no way to change one
  alone: read the current settings, change what you mean to, write the
  whole thing back. A zero `Reminder` clears the lot.
- **`Desk.ID` is the peripheral ID**, not the name passed to `New` — which
  is stored in `Desk.Name`.
- **An unset memory position cannot be told from another unset one.**
  The reply carries nothing identifying which position it answers (see
  the memory positions note above), so distinguishing them needs the same
  serialising of requests as the acknowledgements below.
- **A write acknowledgement cannot be attributed to the write it
  answers.** Every write to the DeskPanel characteristic is acknowledged
  with the same short frame, carrying no command byte — so when more than
  one part of a program writes to that characteristic, an incoming ack
  could belong to any of them. The parsers therefore drop short frames
  rather than guess, which means an operation whose only answer is an ack
  reports nothing back: clearing a memory position succeeds or fails
  silently, and asking for a position that was never set produces no
  "position N is unset". Attributing acks would mean serialising writes to
  the characteristic — a queue holding each write until the previous one's
  ack arrives or times out — which is not implemented. `Desk.TakeOwnership`
  sidesteps it by reading the value back instead. `example/frames -writes`
  shows whether the acks really are indistinguishable.

## Scope

Central/client role over an existing connection. No scanning, no
connecting, no reconnection supervision, no peripheral role — those belong
to whatever owns the `*ble.Client`.

Implemented from the DeskPanel characteristic: capabilities, base offset,
product info, memory positions 1–4, reminder settings and user ID. The
`DeskPanelCommandGetLogEntry` command is defined but does nothing on a
DPG1M.

## See also

- [`corebluetooth-go`](https://github.com/gomi-source/corebluetooth-go) —
  the BLE transport
- [`mqtt-linak`](https://github.com/gomi-source/mqtt-linak) — an MQTT
  bridge built on this, and a worked example of the reconnect and
  ownership handling described above
