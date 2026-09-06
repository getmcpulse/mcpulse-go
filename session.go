package mcpulse

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// One session and one buffer per destination, for the life of the process.
//
// The obvious shape is to make both inside Watch, which is right for a stdio
// server — one process, one Watch, one session — and wrong for an HTTP one. A
// streamable-HTTP server can build a fresh Server per request, so Watch runs
// per request too, and every tool call becomes a session of its own.
//
// That is not a cosmetic difference. Retries are found by looking for the same
// tool twice inside one session, and first-call success is defined as no retry
// following. With one call per session there can never be a retry, so the
// server reports a perfect score however badly it is doing — the one number
// this product exists to tell the truth about.
//
// Keyed by endpoint and key rather than a bare singleton: two watched servers
// reporting to different MCPs in one process are two different streams, and
// merging them would file one customer's calls under another's.

type stream struct {
	sessionID string
	buf       *buffer

	mu sync.RWMutex
	// clientName is whoever most recently identified themselves on initialize.
	//
	// It lives here for the same reason the session id does. As a local in
	// Watch it works for stdio — one Watch, one client — and fails for HTTP
	// exactly as the session did: initialize is handled by one Server and
	// tools/call by the next, so the call never sees the name and every event
	// is filed as "unknown".
	//
	// One value per process per destination, last identification wins. For a
	// server with two concurrent clients that is an approximation, but it is
	// the same approximation the shared session already makes, and a name that
	// is occasionally the other client's beats a column that is always unknown.
	clientName  string
	startupSent bool

	// sink diverts payloads away from the buffer. Only tests set it; in
	// production it is nil and emit goes straight to the batching buffer.
	sink func(Payload)
}

// emit hands one payload to the buffer, or to a test's capture.
func (s *stream) emit(p Payload) {
	if s.sink != nil {
		s.sink(p)
		return
	}
	s.buf.add(p)
}

func (s *stream) client() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientName
}

func (s *stream) setClient(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientName = name
}

// claimStartup reports whether this caller is the one that should send the
// startup payload, and marks it sent.
func (s *stream) claimStartup() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startupSent {
		return false
	}
	s.startupSent = true
	return true
}

var (
	streamsMu  sync.Mutex
	streams    = map[string]*stream{}
	exitOnce   sync.Once
	signalStop chan os.Signal
)

func streamFor(opts resolved, log logger) *stream {
	streamsMu.Lock()
	defer streamsMu.Unlock()

	key := opts.endpoint + "|" + opts.key
	if existing, ok := streams[key]; ok {
		return existing
	}

	created := &stream{
		sessionID:  NewSessionID(),
		buf:        newBuffer(opts, log),
		clientName: "unknown",
	}
	streams[key] = created

	// Registered once for the whole package, however many servers are watched.
	exitOnce.Do(registerExitFlush)

	return created
}

// registerExitFlush arranges one last flush on the way out, so the final few
// calls of a session are not lost to a five-second timer that never fired.
//
// Go has no atexit, and a deferred function in main does not run on a signal.
// Watching SIGINT and SIGTERM is the only way to catch the common shutdown —
// and the signal is forwarded on, so a server that installs its own handler
// still sees it and one that does not still dies.
func registerExitFlush() {
	signalStop = make(chan os.Signal, 1)
	signal.Notify(signalStop, os.Interrupt, syscall.SIGTERM)

	go func() {
		received := <-signalStop
		FlushAll()

		// Hand the signal back so the process ends the way it would have.
		signal.Stop(signalStop)
		if process, err := os.FindProcess(os.Getpid()); err == nil {
			_ = process.Signal(received)
		}
	}()
}

// FlushAll sends everything buffered and stops accepting more.
//
// Call it from a defer in main if the server shuts down without a signal —
// Go cannot hook process exit, so this is the one part of the SDK a Go server
// may have to invoke by hand.
func FlushAll() {
	streamsMu.Lock()
	pending := make([]*stream, 0, len(streams))
	for _, s := range streams {
		pending = append(pending, s)
	}
	streams = map[string]*stream{}
	streamsMu.Unlock()

	var wg sync.WaitGroup
	for _, s := range pending {
		wg.Add(1)
		go func(s *stream) {
			defer wg.Done()
			s.buf.close(exitFlushTimeout)
		}(s)
	}
	wg.Wait()
}
