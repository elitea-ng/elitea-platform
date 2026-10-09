package sharedchat

import (
	"container/list"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// admission.go bounds the work an anonymous unlock request can cause.
//
// Unlock runs a 600_000-round PBKDF2 (pbkdf2Iterations) for a caller who has
// no session. Two limits sit in front of it, both checked BEFORE the store is
// read and before any key is derived:
//
//   - a process-wide cap on verifications running at the same time
//     (verifyGate), so the CPU the KDF can take is bounded however many
//     requests arrive;
//   - an attempt budget per client and link (attemptBudget), so one client
//     cannot use the whole cap, online guessing of one link's password is
//     slow, and callers sharing one address (no trusted proxy configured)
//     cannot spend each other's budget on other links.
//
// Both answer 429 with Retry-After. Both are in memory and per replica: with N
// replicas the effective limits are N times the stated ones.

const (
	// verifySlotsMin and verifySlotsMax bound the concurrent verification
	// cap. One verification is single-threaded and CPU-bound (a few hundred
	// milliseconds), so the cap is half of GOMAXPROCS: the other half stays
	// free for the rest of Main. The ceiling keeps the cap small on very
	// large hosts, where more parallel KDFs add queueing for no benefit.
	verifySlotsMin = 1
	verifySlotsMax = 8

	// verifyBusyRetryAfter is the Retry-After for a request refused because
	// every verification slot is taken: about three verification times.
	verifyBusyRetryAfter = time.Second

	// unlockAttemptsPerWindow is the unlock attempts one client may make in
	// unlockAttemptWindow. Every attempt counts, whatever its outcome, and an
	// attempt refused for lack of a verification slot is given back.
	unlockAttemptsPerWindow = 10
	unlockAttemptWindow     = 5 * time.Minute

	// maxTrackedUnlockClients bounds the budget's memory: past it, expired
	// clients are dropped and then the oldest.
	maxTrackedUnlockClients = 10_000

	// maxClientKeyLength bounds the client part of one tracked key.
	maxClientKeyLength = 64

	// budgetLinkKeyBytes is how much of the token hash names the link in a
	// budget key: 128 bits, enough that two links never share a budget.
	budgetLinkKeyBytes = 16

	// ipv6ClientPrefixBits groups IPv6 clients by the /64 one end site is
	// normally assigned, so rotating through a /64 is one client.
	ipv6ClientPrefixBits = 64

	tooManyAttemptsMessage = "too many password attempts; try again later"
	busyMessage            = "the server is busy; try again shortly"
)

// defaultVerifySlots is the concurrent verification cap for procs CPUs.
func defaultVerifySlots(procs int) int {
	return min(max(procs/2, verifySlotsMin), verifySlotsMax)
}

// verifyGate is a counting semaphore that never waits.
type verifyGate struct{ slots chan struct{} }

func newVerifyGate(n int) *verifyGate {
	return &verifyGate{slots: make(chan struct{}, max(n, verifySlotsMin))}
}

// tryAcquire takes a slot or reports that none is free. The returned release
// must be called exactly once.
func (g *verifyGate) tryAcquire() (release func(), ok bool) {
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, true
	default:
		return nil, false
	}
}

// attemptBudget is a fixed-window attempt counter per client key with a bound
// on the number of keys. Entries sit in a list ordered by window start, so
// the front is always the first to expire.
type attemptBudget struct {
	max        int
	window     time.Duration
	maxClients int
	now        func() time.Time

	mu      sync.Mutex
	order   *list.List // of *attemptEntry, oldest window start first
	clients map[string]*list.Element
}

type attemptEntry struct {
	key   string
	start time.Time
	count int
}

func newAttemptBudget(maxAttempts int, window time.Duration, maxClients int) *attemptBudget {
	return &attemptBudget{
		max: maxAttempts, window: window, maxClients: max(maxClients, 1), now: time.Now,
		order: list.New(), clients: map[string]*list.Element{},
	}
}

// take charges one attempt to key. When the key is over budget it charges
// nothing and returns how long until its window ends.
func (b *attemptBudget) take(key string) (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if el, found := b.clients[key]; found {
		e := el.Value.(*attemptEntry)
		if now.Sub(e.start) >= b.window {
			e.start, e.count = now, 0
			b.order.MoveToBack(el)
		}
		if e.count >= b.max {
			return false, b.window - now.Sub(e.start)
		}
		e.count++
		return true, 0
	}
	for len(b.clients) >= b.maxClients {
		front := b.order.Front()
		if front == nil {
			break
		}
		// The front is the oldest window: expired ones go first, and when
		// none has expired the oldest is dropped to stay within the bound.
		delete(b.clients, front.Value.(*attemptEntry).key)
		b.order.Remove(front)
	}
	b.clients[key] = b.order.PushBack(&attemptEntry{key: key, start: now, count: 1})
	return true, 0
}

// refund gives back one attempt charged by take.
func (b *attemptBudget) refund(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el, found := b.clients[key]; found {
		if e := el.Value.(*attemptEntry); e.count > 0 {
			e.count--
		}
	}
}

func (b *attemptBudget) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

// ClientAddressResolver names the caller of a request. The deployment's
// resolver reads X-Forwarded-For only from a configured trusted proxy and
// otherwise answers the socket peer (internal/api/scim.ClientAddressResolver).
type ClientAddressResolver interface {
	Resolve(*http.Request) (address string, ok bool)
}

// clientKey is the budget key of a request. The resolver's answer is used when
// it has one; otherwise the socket peer is, and X-Forwarded-For is never read
// here. Behind a proxy that is not configured as trusted every caller then
// shares the proxy's key, which is the safe direction: the budget is shared
// instead of bypassed.
func clientKey(r *http.Request, resolver ClientAddressResolver) string {
	address := ""
	if resolver != nil {
		if resolved, ok := resolver.Resolve(r); ok {
			address = resolved
		}
	}
	if address == "" {
		address = r.RemoteAddr
	}
	return normalizeClientKey(address)
}

func normalizeClientKey(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() == "" {
		ip = ip.Unmap()
		if ip.Is6() {
			if prefix, err := ip.Prefix(ipv6ClientPrefixBits); err == nil {
				return prefix.String()
			}
		}
		return ip.String()
	}
	if len(host) > maxClientKeyLength {
		host = host[:maxClientKeyLength]
	}
	return host
}

// retryAfterSeconds renders a Retry-After value, rounded up and at least 1.
func retryAfterSeconds(d time.Duration) string {
	seconds := int((d + time.Second - 1) / time.Second)
	return strconv.Itoa(max(seconds, 1))
}

func currentVerifySlots() int { return defaultVerifySlots(runtime.GOMAXPROCS(0)) }
