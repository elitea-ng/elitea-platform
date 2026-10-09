package sharedchat

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAttemptBudgetAllowsUpToTheBudgetThenRefuses(t *testing.T) {
	b := newAttemptBudget(3, time.Minute, 10)
	for i := 0; i < 3; i++ {
		if ok, _ := b.take("a"); !ok {
			t.Fatalf("attempt %d refused inside the budget", i+1)
		}
	}
	ok, retry := b.take("a")
	if ok {
		t.Fatal("attempt budget+1 was admitted")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry-after = %v, want within (0, window]", retry)
	}
	if ok, _ := b.take("b"); !ok {
		t.Fatal("a different client was refused for another client's attempts")
	}
}

func TestAttemptBudgetWindowExpiryRestoresTheBudget(t *testing.T) {
	now := time.Unix(1_000, 0)
	b := newAttemptBudget(1, time.Minute, 10)
	b.now = func() time.Time { return now }
	b.take("a")
	if ok, _ := b.take("a"); ok {
		t.Fatal("second attempt admitted inside the window")
	}
	now = now.Add(time.Minute)
	if ok, _ := b.take("a"); !ok {
		t.Fatal("attempt refused after the window ended")
	}
}

func TestAttemptBudgetRefundGivesBackOneAttempt(t *testing.T) {
	b := newAttemptBudget(1, time.Minute, 10)
	b.take("a")
	b.refund("a")
	if ok, _ := b.take("a"); !ok {
		t.Fatal("a refunded attempt was not given back")
	}
	b.refund("unknown") // must not create or panic
	if b.size() != 1 {
		t.Fatalf("size = %d, want 1", b.size())
	}
}

func TestAttemptBudgetTrackedClientsNeverExceedTheCap(t *testing.T) {
	const cap = 50
	b := newAttemptBudget(5, time.Hour, cap)
	for i := 0; i < cap*20; i++ {
		b.take(fmt.Sprintf("client-%d", i))
		if got := b.size(); got > cap {
			t.Fatalf("tracked clients = %d after %d distinct keys, cap %d", got, i+1, cap)
		}
		if got := b.order.Len(); got != b.size() {
			t.Fatalf("order list (%d) and map (%d) disagree", got, b.size())
		}
	}
}

func TestAttemptBudgetEvictsTheOldestWindowFirst(t *testing.T) {
	now := time.Unix(1_000, 0)
	b := newAttemptBudget(1, time.Hour, 2)
	b.now = func() time.Time { return now }
	b.take("old")
	now = now.Add(time.Second)
	b.take("new")
	now = now.Add(time.Second)
	b.take("third") // evicts "old"
	if ok, _ := b.take("new"); ok {
		t.Fatal("the newer client lost its counter instead of the oldest")
	}
	if ok, _ := b.take("old"); !ok {
		t.Fatal("the oldest client was still tracked after eviction")
	}
}

func TestDefaultVerifySlotsIsBounded(t *testing.T) {
	for procs, want := range map[int]int{0: verifySlotsMin, 1: verifySlotsMin, 4: 2, 8: 4, 16: verifySlotsMax, 256: verifySlotsMax} {
		if got := defaultVerifySlots(procs); got != want {
			t.Fatalf("defaultVerifySlots(%d) = %d, want %d", procs, got, want)
		}
	}
}

func TestClientKeyIgnoresForwardedForWithoutAResolver(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "198.51.100.7:4444"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientKey(req, nil); got != "198.51.100.7" {
		t.Fatalf("clientKey = %q, want the socket peer host", got)
	}
}

func TestClientKeyGroupsAnIPv6Prefix(t *testing.T) {
	a := normalizeClientKey("[2001:db8:1:2:aaaa::1]:80")
	b := normalizeClientKey("[2001:db8:1:2:bbbb::9]:80")
	if a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("keys = %q, %q; want one /64 key", a, b)
	}
	if got := normalizeClientKey("[::ffff:192.0.2.1]:80"); got != "192.0.2.1" {
		t.Fatalf("mapped key = %q, want the IPv4 address", got)
	}
}

type staticResolver struct {
	addr string
	ok   bool
}

func (s staticResolver) Resolve(*http.Request) (string, bool) { return s.addr, s.ok }

func TestClientKeyUsesTheResolverAndFallsBackToThePeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.0.0.1:1"
	if got := clientKey(req, staticResolver{"203.0.113.9", true}); got != "203.0.113.9" {
		t.Fatalf("resolved key = %q", got)
	}
	if got := clientKey(req, staticResolver{"", false}); got != "10.0.0.1" {
		t.Fatalf("unresolved key = %q, want the socket peer", got)
	}
}
