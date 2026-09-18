package desk

import (
	"context"
	"fmt"

	"github.com/gomi-source/linak-dpg"
)

// The DeskPanel characteristic is request/response wearing a
// subscription's clothes: you ask for a parameter and the desk answers
// once. It only arrives as a notification because that is how GATT
// delivers it. So the API over it is synchronous - BaseOffset, User,
// Reminder and the rest - and this file is the machinery underneath.
//
// Two facts shape it.
//
// A reply carries nothing identifying the request it answers. Different
// parameters answer under different response codes, so those are never
// confused, but two reads of the same parameter in flight are
// indistinguishable, and a memory position reply says nothing about which
// position it describes. The only fix is to allow one query at a time,
// which is what deskPanelMu enforces. It is load-bearing, not defensive.
//
// The desk never speaks first. Storing a favourite from the panel or
// activating a reminder - the only change the panel itself can make -
// pushes no frame, so nothing arrives that is not an answer to something.
// That is what makes a synchronous API the whole story here rather than
// half of it. It was established by example/frames -listen; if a controller
// is ever found that does push, query would have to match replies to
// requests by time rather than take the next one.

// slot holds the most recent value reported for one response code. It
// keeps one value, not a queue: a stale reading is never what a caller
// wants, and the dispatch loop must not block on a reader that has gone
// away.
type slot[T any] struct {
	ch chan T
}

func newSlot[T any]() *slot[T] { return &slot[T]{ch: make(chan T, 1)} }

// put stores a value, displacing any unread one.
func (s *slot[T]) put(v T) {
	select {
	case s.ch <- v:
		return
	default:
	}

	select {
	case <-s.ch:
	default:
	}
	select {
	case s.ch <- v:
	default:
		// Another notification beat us to it; its value is at least as
		// fresh as ours, so there is nothing to fix.
	}
}

// drain discards anything reported before the request we are about to
// send, so a stale value cannot be mistaken for the answer.
func (s *slot[T]) drain() {
	select {
	case <-s.ch:
	default:
	}
}

// deskPanelSlots is every answer the desk can give on the DeskPanel
// characteristic. Each is filled by one callback registered once, for the
// Desk's whole lifetime, when the subscription is created - the notify
// registry cannot drop a subscription's dispatcher once started, and
// registering per query would also race the dispatch loop, which reads the
// callback map without a lock.
type deskPanelSlots struct {
	user         *slot[[]byte]
	baseOffset   *slot[int]
	capabilities *slot[dpg.Capabilities]
	productInfo  *slot[dpg.ProductInfo]
	reminder     *slot[dpg.Reminder]
	memory       *slot[int]
	memoryUnset  *slot[struct{}]
}

func newDeskPanelSlots() *deskPanelSlots {
	return &deskPanelSlots{
		user:         newSlot[[]byte](),
		baseOffset:   newSlot[int](),
		capabilities: newSlot[dpg.Capabilities](),
		productInfo:  newSlot[dpg.ProductInfo](),
		reminder:     newSlot[dpg.Reminder](),
		memory:       newSlot[int](),
		memoryUnset:  newSlot[struct{}](),
	}
}

// query sends one request and waits for the answer it belongs to.
//
// It holds deskPanelMu for the whole exchange, so only one DeskPanel
// question is outstanding at a time. That is what makes the answer
// attributable: without it, two reads of the same parameter would race for
// each other's replies.
func query[T any](ctx context.Context, d *Desk, s *slot[T], what string, send func() error) (T, error) {
	var zero T

	if d.device.IsZero() {
		return zero, fmt.Errorf("could not read %s: desk has no device", what)
	}

	// The subscription has to exist before the request goes out, or the
	// answer has nowhere to land.
	d.InitDeskPanelSubscription()

	d.deskPanelMu.Lock()
	defer d.deskPanelMu.Unlock()

	s.drain()
	if err := send(); err != nil {
		return zero, err
	}

	select {
	case v := <-s.ch:
		return v, nil
	case <-ctx.Done():
		return zero, fmt.Errorf("gave up waiting for the desk to report its %s: %w", what, ctx.Err())
	}
}

// withTimeout applies DefaultTimeout when the caller passed a context with
// no deadline of its own, so a dropped link cannot hang a read forever.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultTimeout)
}
