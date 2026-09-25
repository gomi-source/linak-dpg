# Pairing and connecting

Everything here was seen on a DPG1M with macOS, through `corebluetoothd`.
None of it is under this package's control: pairing, and who holds a
desk's connection, belong to macOS and the desk. What the package can do is
fail in a recognisable way, and this is the key to reading those failures.

## The Connection Request dialog

Shortly after a connection is made, macOS shows a **Connection Request**
dialog for the desk: *Connect* or *Cancel*. It is pairing, and it is
macOS asking, not this package — CoreBluetooth has no option that stops
it.

**Moving needs it.** On a desk that is not paired with this Mac,
everything short of a move works: `TakeOwnership` reads back the owner
bit it wrote, so DeskPanel writes are accepted, and wake-up and stop raise
no error. But heights written to ReferenceInput are dropped, and the desk
does not move. It says nothing about it, because it cannot:
ReferenceInput is write-without-response, so there is no reply for a
refusal to travel in. Presumably the controller acts on a height only
over an encrypted link. The result looks exactly like missing ownership
— see [Ownership](README.md#ownership) — and is told apart by the dialog, or by the desk
missing from this Mac's known devices.

What the dialog's three outcomes leave:

- ***Connect*** pairs the desk, and moves work.
- ***Cancel*** leaves it unpaired: reads, ownership and Control commands
  work, moves do not, and the dialog comes back on the next connect. (The
  "ignore this device" checkbox behaves like *Cancel*.)
- **Left unanswered**, the dialog withdraws itself after a while,
  whatever the program does in the meantime. Until then requests stall,
  and since the dialog outlasts this package's own request timeout, one
  caught behind it fails first — `context deadline exceeded` from
  `GetCharacteristic`. The next connect succeeds, but the desk is still
  unpaired, so moves still do nothing.

## A desk connected to another Mac

**A desk connected to another Mac looks available and is not.** It goes
on advertising, so it shows up in a scan, in `mqtt-linak -scan`, in
Bluetility — and every connect attempt simply times out:

```
connect: corebluetoothd: rpc error 2: timeout: connect timed out
```

CoreBluetooth reports neither success nor failure because the desk never
answers, which is why every tool on the Mac trying to connect fails the
same way and none of them says why.

Pairing is not what holds a desk. A desk has been paired with two Macs
at once and moved for whichever was connected, and on a paired Mac it
shows as *not connected* in System Settings once the program has
disconnected. What holds it is a live connection — and that need not be
visible: once, a Mac kept the desks connected after its bridge had been
stopped, and went on reconnecting them, when it never had before; what
did it was not established. So when a desk can be seen but not connected
to, look for another Mac connected to it, check there for anything still
running (`pgrep -fl 'corebluetoothd|mqtt-linak'`, and a launchd job that
restarts what was stopped), and failing that, forget the desk there
(System Settings → Bluetooth → ⓘ → Forget This Device) or turn that Mac's
Bluetooth off — either releases it at once. Forgetting it on the Mac that
cannot connect does nothing, and neither does restarting `bluetoothd`
there. A dialog waiting on this Mac fails differently: the connect
succeeds and a later request times out.

## Disconnect loops after re-pairing

After a desk was forgotten on this Mac (System Settings → Bluetooth → ⓘ →
Forget This Device) and paired again through the dialog, both desks went
into a loop of disconnecting and reconnecting. Putting each desk into
pairing mode from its panel ended it, while the loop was running: the
desk was not forgotten again and the bridge was not restarted. The desks
have behaved since.

Why is not established. A plausible reading is that the desk still held
the keys from the old pairing, and pairing mode let it take the new ones —
so after forgetting a desk and pairing it again, pair it from the desk's
side too.
