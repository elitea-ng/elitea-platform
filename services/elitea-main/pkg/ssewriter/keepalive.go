package ssewriter

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// KeepaliveInterval is how long an SSE response may stay silent before a
// Keepalive writes a comment into it.
//
// The bound is the idle timeout of whatever sits between the browser and
// elitea-main. Cloudflare-style proxies close an origin connection that sends
// nothing for 100 seconds; a GKE load balancer's backend timeout is 30 seconds
// unless the chart's BackendConfig raises it. A stream that waits on a model or
// a tool can be silent for longer than either, and the client then sees a
// dropped connection where the server saw nothing wrong. 25 seconds keeps the
// worst case (one and a half intervals, see Keepalive) under 40.
const KeepaliveInterval = 25 * time.Second

// keepaliveComment is the line written. A line that starts with `:` is a
// comment in the SSE grammar: every conforming parser drops it, including the
// OpenAI SDKs' and libs/rust/model-client's.
var keepaliveComment = []byte(": keepalive\n\n")

// Keepalive serialises the writes to one SSE response and, while the response
// is idle, writes a comment every interval so no proxy idles it out.
//
// It writes a comment ONLY at an event boundary — when the bytes written so
// far end in a blank line, or nothing has been written through it yet. A
// relayed stream (the /llm passthrough) can stop mid-event, and a comment
// spliced into the middle of an event would corrupt it.
//
// Start it on a response whose head is sent and whose body so far ends at an
// event boundary, route every later body write through Write, and call Stop
// before the handler writes anything else or returns.
type Keepalive struct {
	mu         sync.Mutex
	w          http.ResponseWriter
	controller *http.ResponseController
	interval   time.Duration
	last       time.Time
	tail       []byte
	failed     bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// StartKeepalive starts the idle ticker on w. An interval of zero or less
// means KeepaliveInterval.
func StartKeepalive(w http.ResponseWriter, interval time.Duration) *Keepalive {
	if interval <= 0 {
		interval = KeepaliveInterval
	}
	k := &Keepalive{
		w:          w,
		controller: http.NewResponseController(w),
		interval:   interval,
		last:       time.Now(),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	go k.run()
	return k
}

// Write writes p to the response and flushes it, so a relayed stream is not
// re-buffered on this hop.
func (k *Keepalive) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n, err := k.w.Write(p)
	k.remember(p[:n])
	if err != nil {
		k.failed = true
		return n, err
	}
	k.last = time.Now()
	if err := k.controller.Flush(); err != nil {
		k.failed = true
		return n, err
	}
	return n, nil
}

// Stop ends the ticker and waits for it, so no comment is written after Stop
// returns. It is safe to call more than once.
func (k *Keepalive) Stop() {
	k.stopOnce.Do(func() { close(k.stop) })
	<-k.done
}

// run ticks at half the interval, so the longest silence is one and a half
// intervals: a write just after a tick defers the comment to the tick after
// next.
func (k *Keepalive) run() {
	defer close(k.done)
	ticker := time.NewTicker(k.interval / 2)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			if !k.beat() {
				return
			}
		}
	}
}

// beat writes one comment when the response has been idle for the interval
// and sits at an event boundary. It reports false once the response is dead.
func (k *Keepalive) beat() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failed {
		return false
	}
	if time.Since(k.last) < k.interval || !k.atBoundary() {
		return true
	}
	if _, err := k.w.Write(keepaliveComment); err != nil {
		k.failed = true
		return false
	}
	if err := k.controller.Flush(); err != nil {
		k.failed = true
		return false
	}
	k.last = time.Now()
	return true
}

// remember keeps the last four bytes written, which is enough to see every
// spelling of the blank line that ends an event.
func (k *Keepalive) remember(p []byte) {
	k.tail = append(k.tail, p...)
	if len(k.tail) > 4 {
		k.tail = append(k.tail[:0], k.tail[len(k.tail)-4:]...)
	}
}

// atBoundary reports whether the body so far ends an event: nothing yet, or a
// blank line in any of the SSE line-ending spellings (LF, CR, CRLF).
func (k *Keepalive) atBoundary() bool {
	if len(k.tail) == 0 {
		return true
	}
	return bytes.HasSuffix(k.tail, []byte("\n\n")) ||
		bytes.HasSuffix(k.tail, []byte("\r\r")) ||
		bytes.HasSuffix(k.tail, []byte("\r\n\r\n"))
}
