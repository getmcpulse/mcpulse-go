package mcpulse

import (
	"context"
	"sync"
	"time"
)

// buffer holds payloads and sends them in batches, on a goroutine of its own.
//
// The contract with the tool call that produced a payload is that add returns
// immediately and never blocks. The wake channel is buffered with room for one
// and every send to it is a select-with-default, so a request path can never
// wait on the sender — that is the whole of rule 2, and it is the kind of thing
// that looks fine under test and deadlocks under load.
type buffer struct {
	opts resolved
	log  logger

	mu      sync.Mutex
	pending []Payload
	closed  bool

	// sending is held for the duration of a batch, so flush can wait for a real
	// send rather than for the queue merely being emptied.
	sending sync.Mutex

	wake chan struct{}
	done chan struct{}
	once sync.Once
}

func newBuffer(opts resolved, log logger) *buffer {
	b := &buffer{
		opts: opts,
		log:  log,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	go b.run()
	return b
}

// add buffers one payload. It returns immediately and never blocks.
func (b *buffer) add(p Payload) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}

	if len(b.pending) >= maxBuffered {
		// Oldest first: recent calls describe what the server is doing now, and
		// that is the more useful half of a buffer that could not be sent.
		b.pending = b.pending[1:]
		b.log("buffer full, dropped oldest payload")
	}

	b.pending = append(b.pending, p)
	ready := len(b.pending) >= flushAtItems
	b.mu.Unlock()

	if ready {
		b.signal()
	}
}

// signal asks the sender to run now, without ever blocking the caller.
func (b *buffer) signal() {
	select {
	case b.wake <- struct{}{}:
	default: // A wake-up is already pending; one is enough.
	}
}

func (b *buffer) run() {
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			b.sendOnce(context.Background())
			return
		case <-b.wake:
			b.sendOnce(context.Background())
		case <-ticker.C:
			b.sendOnce(context.Background())
		}
	}
}

func (b *buffer) sendOnce(ctx context.Context) {
	b.sending.Lock()
	defer b.sending.Unlock()

	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	// Taken in one go: anything added while this is in flight belongs to the
	// next batch, not this one.
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if postBatch(ctx, batch, b.opts, sendTimeout) {
		b.log("sent %d payloads", len(batch))
	} else {
		b.log("dropped %d payloads", len(batch))
	}
}

// close performs one final, best-effort flush. After it the buffer accepts
// nothing more.
func (b *buffer) close(timeout time.Duration) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()

	b.once.Do(func() { close(b.done) })

	// Wait for the sender to finish the last batch, but never longer than the
	// exit window: holding a customer's shutdown open over analytics is the
	// same failure as holding their request path open.
	finished := make(chan struct{})
	go func() {
		b.sending.Lock()
		b.sending.Unlock() //nolint:staticcheck // waiting, not guarding
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(timeout):
		b.log("exit flush timed out")
	}
}
