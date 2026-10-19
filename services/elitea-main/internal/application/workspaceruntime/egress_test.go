package workspaceruntime

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidateProxyURL(t *testing.T) {
	for _, ok := range []string{"http://elitea-egress-proxy.platform.svc:3128", "http://10.0.0.5:3128", "http://[fd00::5]:3128", "http://egress-proxy:3128/"} {
		if err := ValidateProxyURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "https://proxy:3128", "http://proxy", "http://run:x@proxy:3128", "http://proxy:3128/path", "http://proxy:3128?q=1", "http://proxy:0", "http://proxy:70000", "proxy:3128", "http://:3128"} {
		if err := ValidateProxyURL(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if err := (Egress{ProxyURL: "http://user:pw@proxy:3128"}).Validate(); err == nil {
		t.Error("a proxy URL with credentials must fail Validate")
	}
	if err := (Egress{}).Validate(); err != nil {
		t.Errorf("the zero Egress is valid: %v", err)
	}
	if err := (Egress{DefaultTurnWall: 25 * time.Hour}).Validate(); err == nil {
		t.Error("a default wall over 24h must be refused")
	}
}

func TestEgressTokenTTL(t *testing.T) {
	e := Egress{}
	if got := e.tokenTTL(Policy{}); got != DefaultEgressTurnWall+EgressTokenGrace {
		t.Errorf("default: %s", got)
	}
	e.DefaultTurnWall = 10 * time.Minute
	if got := e.tokenTTL(Policy{}); got != 10*time.Minute+EgressTokenGrace {
		t.Errorf("configured default: %s", got)
	}
	if got := e.tokenTTL(Policy{TurnWallSeconds: 60}); got != time.Minute+EgressTokenGrace {
		t.Errorf("policy wall: %s", got)
	}
	if got := e.tokenTTL(Policy{TurnWallSeconds: 23*3600 + 3599}); got != maxEgressTTL {
		t.Errorf("capped at 24h: %s", got)
	}
}

type recordingMinter struct {
	calls int
	ttl   time.Duration
}

func (m *recordingMinter) Mint(_ string, _ int64, _ []string, ttl time.Duration) (string, error) {
	m.calls++
	m.ttl = ttl
	return "tok", nil
}

// A token is minted only for an allowlist turn; network off and direct mode
// carry none.
func TestEgressForMintsOnlyInProxyMode(t *testing.T) {
	minter := &recordingMinter{}
	e := Egress{ProxyURL: "http://proxy:3128", Minter: minter}
	claim := TurnClaim{ExecutionID: "x", ProjectID: 1}
	for _, tc := range []struct {
		policy    Policy
		allowlist []string
		mode      string
	}{
		{Policy{Network: false}, nil, EgressModeNone},
		{Policy{Network: true}, nil, EgressModeDirect},
		{Policy{Network: true}, []string{"pypi.org"}, EgressModeProxy},
	} {
		got, err := e.egressFor(claim, tc.policy, tc.allowlist)
		if err != nil || got.Mode != tc.mode {
			t.Fatalf("%+v: %v %v", tc, got, err)
		}
		if (got.Token != "") != (tc.mode == EgressModeProxy) {
			t.Fatalf("token presence for %s", tc.mode)
		}
	}
	if minter.calls != 1 {
		t.Fatalf("minted %d times", minter.calls)
	}
	if s := fmt.Sprintf("%v %+v %#v", Egress{}, RuntimeContextEgress{Token: "secret-token"}, RuntimeContextEgress{Token: "secret-token"}); strings.Contains(s, "secret-token") {
		t.Fatalf("token printed: %s", s)
	}
}
