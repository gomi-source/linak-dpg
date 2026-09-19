package subscription

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Registering, removing and dispatching all touch the same map. Before
// these were serialised, a callback added while a notification was being
// dispatched was not just a data race: ranging over a map that another
// goroutine writes is a fatal runtime error, so it would have taken the
// process down. Run with -race.
func TestCallbackMapIsSafeUnderConcurrency(t *testing.T) {
	s := &Subscription{}
	const key = byte(MessageTypeDeskPanelBaseOffset)

	var calls atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// A dispatcher, doing what the dispatch loop does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, h := range s.handlersFor(key) {
				h([]byte{1, 3, 0})
			}
		}
	}()

	// Registrations and removals racing it.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				id := s.register(key, func([]byte) { calls.Add(1) })
				s.unregister(key, id)
			}
		}()
	}

	// And a handler that registers another from inside itself, which is
	// the case that would have mutated the map mid-range.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			id := s.register(key, func([]byte) {
				inner := s.register(key, func([]byte) {})
				s.unregister(key, inner)
			})
			s.unregister(key, id)
		}
	}()

	close(stop)
	wg.Wait()
}

func TestRegisterAndUnregister(t *testing.T) {
	s := &Subscription{}
	const key = byte(MessageTypeDeskPanelUserID)

	if got := s.handlersFor(key); got != nil {
		t.Fatalf("a fresh subscription reported %d handlers", len(got))
	}

	var a, b int
	idA := s.register(key, func([]byte) { a++ })
	idB := s.register(key, func([]byte) { b++ })

	if got := len(s.handlersFor(key)); got != 2 {
		t.Fatalf("got %d handlers, want 2", got)
	}

	s.unregister(key, idA)
	for _, h := range s.handlersFor(key) {
		h(nil)
	}
	if a != 0 {
		t.Error("a removed handler was still called")
	}
	if b != 1 {
		t.Errorf("remaining handler called %d times, want 1", b)
	}

	// Removing twice, and removing from an empty message type, are no-ops
	// rather than panics: a caller holding a stale remove function is a
	// normal thing, not a bug to crash on.
	s.unregister(key, idA)
	s.unregister(key, idB)
	s.unregister(byte(MessageTypeControlError), 99)

	if got := s.handlersFor(key); got != nil {
		t.Fatalf("got %d handlers after removing both, want none", len(got))
	}
}

// handlersFor must hand back a copy: the dispatch loop calls what it gets
// without the lock, so the map must not be reachable from it.
func TestHandlersForReturnsACopy(t *testing.T) {
	s := &Subscription{}
	const key = byte(MessageTypeReferenceOutput)

	id := s.register(key, func([]byte) {})
	handlers := s.handlersFor(key)
	s.unregister(key, id)

	if len(handlers) != 1 {
		t.Fatalf("the snapshot changed under us: %d handlers", len(handlers))
	}
}
