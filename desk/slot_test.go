package desk

import (
	"bytes"
	"testing"
)

func TestSlotHoldsOneValue(t *testing.T) {
	s := newSlot[int]()
	s.put(1)
	s.put(2)

	if got := <-s.ch; got != 2 {
		t.Fatalf("held %d, want the newer reading", got)
	}
	select {
	case extra := <-s.ch:
		t.Fatalf("slot should hold one value, also found %d", extra)
	default:
	}
}

// The dispatch loop must never be blocked by a slot nobody is reading.
func TestSlotPutDoesNotBlock(t *testing.T) {
	s := newSlot[int]()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.put(i)
		}
		close(done)
	}()
	<-done
}

func TestSlotDrain(t *testing.T) {
	s := newSlot[int]()
	s.put(7)
	s.drain()
	select {
	case v := <-s.ch:
		t.Fatalf("slot should be empty after draining, found %d", v)
	default:
	}
	s.drain() // draining an empty slot must not block
}

// The user ID arrives in a buffer the dispatch loop reuses, so what lands
// in the slot has to be a copy. Aliasing it would make the owner bit read
// as whatever the next frame happened to carry.
func TestPutUserStoresACopy(t *testing.T) {
	d := &Desk{slots: newDeskPanelSlots()}
	data := []byte{0, 1, 2, 3}
	d.putUser(data)
	data[0] = 0xFF

	got := <-d.slots.user.ch
	if !bytes.Equal(got, []byte{0, 1, 2, 3}) {
		t.Fatalf("stored %v, want an unaliased copy of the original", got)
	}
}
