package wait

import "sync"

// Wait bridges the propose and apply paths. A handler registers a channel
// before proposing; the apply loop triggers the channel when the committed
// entry is processed.
type Wait interface {
	// Register returns a chan that waits on the given ID.
	// The chan will be triggered when Trigger is called with the same ID.
	Register(id uint64) <-chan any
	// Trigger sends the result to the waiter for the given ID and removes
	// the entry. If the ID is not registered, it is a no-op.
	Trigger(id uint64, x any)
	// IsRegistered returns whether a waiter exists for the given ID.
	IsRegistered(id uint64) bool
}

type waitList struct {
	mu sync.Mutex
	m  map[uint64]chan any
}

func New() Wait {
	return &waitList{m: make(map[uint64]chan any)}
}

func (w *waitList) Register(id uint64) <-chan any {
	ch := make(chan any, 1)
	w.mu.Lock()
	w.m[id] = ch
	w.mu.Unlock()
	return ch
}

func (w *waitList) Trigger(id uint64, x any) {
	w.mu.Lock()
	ch, ok := w.m[id]
	if ok {
		delete(w.m, id)
	}
	w.mu.Unlock()
	if ok {
		ch <- x
	}
}

func (w *waitList) IsRegistered(id uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.m[id]
	return ok
}
