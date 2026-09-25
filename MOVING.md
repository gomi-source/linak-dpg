# Moving the desk

How `Desk.Move` drives a DPG controller, and the measurements behind each
rule. Everything here was measured on a DPG1M with `example/moves`;
`go run ./example/moves -list` describes the scenarios, and the one that
produced a figure is named beside it.

A DPG desk moves only while it is being told where to go: the target
height is written to ReferenceInput again and again until the desk
arrives. So `Move` is a loop, not a write, and most of what follows is
about what that loop must and must not write, and when.

## The basics

`Move` returns as soon as the first target has been written. A background
goroutine then rewrites it until the desk arrives, and gives up after 5
seconds without progress.

**A halt is not an arrival, and it is not pushed through.** The desk
reports speed 0 both when it has reached the target and when it has
stopped short — and one reason it stops short is that it has hit
something. Arrival is *speed 0 and within the controller's dead band of
the target* — 1.2 mm, measured: a DPG1M moves for a 1.3 mm request and
not for 1.2 mm. A halt
anywhere else, once the desk has been moving, **ends the move**: the
target is not written again. The controller's own safety stop is the only
protection whatever it met has, and re-commanding would drive the desk
back into it. The move logs where it stopped and how far short; whether to
ask again belongs to the caller, who may know the way is clear.

Before the desk has moved at all, the target is offered again a full
`RetargetGap` after the last write, for up to 2.5 s — three writes at the
default gap — and then given up on with a warning. That is the one place a
command is repeated at a desk that is not moving, and it is bounded for
the reason above. The spacing matters as much as the limit: see *A second of silence before
a new height* below.

**Nothing acknowledges a write.** ReferenceInput is
write-without-response, so a target that never reached the controller
looks exactly like one it chose to ignore. Once the desk is moving this
costs nothing — its reports drive the loop and the next one re-sends the
target anyway — but from rest there are no reports, so a single write that
went missing would end the move in silence. The retry above is for the
link, not for the desk's decision: it stops the instant the desk has
moved, after which a halt is the desk's own and is never re-commanded.

**The desk reports only while it moves.** A stationary one says nothing
at all — no position, no speed 0, nothing. So silence is the normal state
between moves, and it is also how a move that never started announces
itself: there is no reading to classify, so the move ends when the start
window above closes rather than at a speed 0 reading. The warning carries
the last height the desk reported, which may be from an earlier move, and
the distance from it. The five-second stall timeout is left for the other
case — reports that stop mid-move, which means the link went away.

**A move to where the desk already is returns immediately.** The `Desk`
follows the height continuously once the first move has run — the recorder
is registered once and never removed, so it also sees a move made from the
panel — and a target within the dead band of the last known position is
recognised as a no-op rather than written and waited on. Without that it
costs five seconds of silence and a warning for a move that was never
going to happen.

When a move does time out, the remaining explanations are the dead band
(with an unknown position, so compare the logged distance), ownership, and
pairing.
"A height was written too recently" is not among them: the gap below makes
that state unreachable.

`arrivalTolerance` is that dead band, and it answers both questions
because they are the same fact: a target this close is a no-op, and a desk
stopped this close has arrived, since it cannot get nearer. **Too large is
the dangerous direction** — set above the dead band, it makes real moves
report arrival before they start, and then nothing sustains them.

## A second of silence before a new height

**The desk needs a second of silence before a new height.** A height
that arrives too soon after the previous one is ignored — no movement,
no error, no reports. `example/moves -scenarios threshold` measured it by
bringing the desk to rest, staying silent for a set time and writing one
height:

| Silence before the new height | Moved |
|---|---|
| 500–700 ms | 0 of 6 |
| 800 ms | 1 of 2 |
| 900 ms – 2.5 s | 14 of 14 |

The direction of either move made no difference. The clock appears to
run from the last height the controller received and to restart on
every one: a desk sent a height too soon, then offered it again every
200 ms, stayed deaf for as long as that went on. So `RetargetGap` — the
silence `Move` keeps before the first height of a move, after a
retarget, and between re-offers — is 1 s.

It used to be 800 ms, which is where the intermittent failures to move
came from: a move following another closely was a coin flip, which is
why two retargets with timings matching to within 2 ms once went
opposite ways, and why no fixed rule seemed to fit. Two other
explanations were tried and are dead — a controller that disarms at rest
(a wake-up before each move did not help) and lost writes (they may
happen, and the re-offer covers them, but they were not the cause).

The time of the last height written is remembered on the `Desk` across
moves, so the gap holds between moves as well as within one: a `Move`
issued half a second after the previous one finished waits out the rest
of the second before writing anything.

`RetargetGap` (1 s) is the floor under that wait, not the wait itself:
the retarget resumes once the desk is at rest *and* the gap has elapsed,
whichever is later. At full speed the rest comes later — about 1.25 s —
so the gap only binds when the desk was already slowing. It is a package
variable, so another controller can be given another figure.

Anything the desk reports during that gap — including speed 0 — is the old
move ending, not a fault. The halt in the middle of a retarget is ours;
only a halt during travel is the desk's own.

## Changing target mid-move

Calling `Move` again while moving changes where the move goes rather
than starting a second one. **The desk only accepts a different height
once it has come to rest.** Writing one sooner halts it instead of
redirecting it — in any direction. `example/moves -scenarios extend`
bypassed `Move` and switched a desk at full speed to a second height,
sustaining the new one exactly as `Move` sustains a target:

| Second height | Up | Down |
|---|---|---|
| further on than the first | halted, 2 of 2 | halted, 2 of 2 |
| nearer than the first | halted, 2 of 2 | halted, 2 of 2 |

Every trial came to rest 8–10 mm after the switch and sent `01 00 10`
followed by an empty frame, just as a halted reversal does. There is
no redirecting a moving desk.

**So a target further on in the same direction is queued, not
retargeted.** `Move` keeps the current height in front of the desk,
notes the new one, and carries on to it once the desk arrives — the
desk was going to stop there anyway, and it has been travelling the
right way all along. A later further target replaces the queued one,
so 0 → 1000, then 1500 and 2000 on the way, stops once, at 1000, and
then goes to 2000. Asking for the current target again drops the
queued one. A stop short of the leg — an obstruction — drops it too,
as it ends the move. Nearer targets and reversals cannot wait for the
end of the leg, since the desk would run past them, so they retarget as
described next.

Rewriting the *same* height is what sustains movement, and stopping the
writes is what stops the desk — so a retarget goes quiet, waits for the
stream to report speed 0, and only then writes the new height. The silence
is the braking, and it is slow: a DPG1M runs on at full speed for about
0.85 s after the last height, then takes about 0.4 s to stop, so a retarget
at full speed overshoots by some 40 mm. `Stop` does not shorten that —
`example/moves -scenarios stopcoast` measured a `Stop` sent at the moment
of a reversal and found the rest time and the overrun unchanged — so a
reversal is braked like any other retarget. `Stop` evidently applies to
movement driven by the Control commands, not to a desk heading for a
height.

Writing the new height at once *does* brake it. With the experimental
`desk.HaltOnRetarget` on, the same scenario measured, averaged over three
reversals each:

| | At rest after | Overrun | Heading back after |
|---|---|---|---|
| coasting (default) | 1.27 s | 39 mm | 1.41 s |
| halting write | 0.51 s | 10 mm | 1.19 s |

Every halted trial turned and arrived. But each halt also made the error
characteristic send `01 00 10`, 60–90 ms after the halting write, and an
empty frame about 0.8 s after that. Heading back is
only 0.2 s sooner, because the rewrite that starts the new move still
waits a full `RetargetGap` from the halting write. What the code means,
and whether repeated halts ever leave it set, is not known — so
`HaltOnRetarget` stays off. With it on, `Move` treats that one code, in
that one window, as expected rather than as a fault.

**A burst of targets costs one gap, not one each.** Targets arriving while
the desk is quiet — during a retarget, or at the end of a leg on the way
to a queued one — supersede the one that started the gap, so `1500, 1700,
1900, 2100` sent in quick succession interrupts the desk once and then
runs to 2100 — rather than pausing a gap per nudge. The direction check
is made after the gap against the final target, so a burst that ends up
pointing the other way is still stopped properly first.

**The same destination again is not a retarget at all.** A consumer
republishing a value it already sent is ordinary — a retained message
redelivered, a UI echoing its own state — and acting on it would stop a
move that is already going where it is asked to. Near the end of a move
that is actively harmful: the stop leaves a remainder inside the dead
band, so the restart reports `desk did not start moving` and the desk
finishes short of a target it would otherwise have reached. A repeat
within the dead band of the current target is therefore ignored, and a
burst that happens to end where it began resumes the move rather than
starting a new one.

## Collisions

**A collision ends the move the moment the desk reports it.**
`example/moves -scenarios blocked` drove a DPG1M down at full speed into
a block of wood. The error characteristic sent `01 00 3B`, the position
reports showed a stall (`02 00` in the speed word), and then the desk
backed away by itself with bit 1 of every speed word set, until a final
`00 00` at rest. The back-off is a few centimetres, not a return to
where the move began:

| Move | Stalled at | Came to rest at | Backed off | Took |
|---|---|---|---|---|
| 1499 → 1000 | 1177 | 1499 | 32 mm | 1.6 s |
| 2501 → 1000 | 1326 | 1723 | 40 mm | 1.8 s |

(The first run coming to rest exactly where it started was a
coincidence of distances.) The recovery is a move of its own —
accelerating to full speed and braking — and the desk ignored every
height written to it throughout. Before this change `Move` kept writing
them until the desk came to rest, and a height the desk did accept once
the recovery was over would have sent it straight back into the
obstruction. So `Move` now ends on the first sign: an error frame from
`Desk.ControlErrors`, or a report with `referenceoutput.FlagCollisionRecovery`
set. Nothing more is written; the desk finishes its recovery by itself.
The same goes for any error frame other than the one a halting write
provokes on purpose (see *Changing
target mid-move*), including codes never seen before. A desk
or helper without the error characteristic still gets the flag check.

## Internals

Position reports go into a one-slot holder, never a queue. They arrive on
their own goroutine each, so a blocking handoff leaves a backlog of
readings from seconds ago — and a speed 0 recorded *before* a retarget
then surfaces *after* it and reads as the new move finishing. That is a
real failure, not a theoretical one: a short first move that completes
while the retarget is pending ends the whole sequence one write into the
second move.
