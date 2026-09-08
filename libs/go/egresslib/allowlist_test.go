package egresslib

import (
	"strings"
	"testing"
)

func TestParseEntryAcceptsTheGrammar(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw      string
		wantKind Kind
		wantText string
	}{
		{"example.com", KindExact, "example.com"},
		{"  EXAMPLE.com  ", KindExact, "example.com"},
		{"example.com.", KindExact, "example.com"},
		{"example.com:8000", KindExact, "example.com:8000"},
		{"192.168.29.60:8000", KindExact, "192.168.29.60:8000"},
		{"[::1]:8000", KindExact, "[::1]:8000"},
		{"0:0:0:0:0:0:0:1", KindExact, "::1"},
		{"*.example.com", KindWildcard, "*.example.com"},
		{"*.openai.azure.com:443", KindWildcard, "*.openai.azure.com:443"},
		{"192.168.29.0/24", KindCIDR, "192.168.29.0/24"},
		{"10.0.0.0/8", KindCIDR, "10.0.0.0/8"},
		{"fd00::/8", KindCIDR, "fd00::/8"},
		// A host bit set inside a CIDR is normalised to the network address, so
		// two spellings of one block deduplicate.
		{"192.168.29.60/24", KindCIDR, "192.168.29.0/24"},
	}
	for _, tc := range cases {
		entry, err := ParseEntry(tc.raw)
		if err != nil {
			t.Fatalf("ParseEntry(%q): unexpected error %v", tc.raw, err)
		}
		if entry.Kind() != tc.wantKind {
			t.Errorf("ParseEntry(%q).Kind() = %q, want %q", tc.raw, entry.Kind(), tc.wantKind)
		}
		if entry.String() != tc.wantText {
			t.Errorf("ParseEntry(%q).String() = %q, want %q", tc.raw, entry.String(), tc.wantText)
		}
	}
}

func TestParseEntryRefusesMalformedInput(t *testing.T) {
	t.Parallel()

	// Each of these is refused rather than skipped. A silently dropped rule
	// either opens the gateway wider than intended or wedges a provider.
	cases := []string{
		"",
		"   ",
		"https://example.com",
		"example.com/v1",
		"example .com",
		"example.com:0",
		"example.com:70000",
		"example.com:notaport",
		"*",
		"*.",
		"*.*.example.com",
		"exa*ple.com",
		"192.168.0.0/33",
		"notacidr/24",
	}
	for _, raw := range cases {
		if _, err := ParseEntry(raw); err == nil {
			t.Errorf("ParseEntry(%q): want an error, got none", raw)
		}
	}
}

func TestParseRejectsTheWholeSetOnOneBadEntry(t *testing.T) {
	t.Parallel()

	if _, err := Parse([]string{"good.example.com", "https://bad"}); err == nil {
		t.Fatal("Parse: want an error for a malformed entry, got none")
	}
	// A blank member is a trailing comma in a CSV environment variable, not an
	// authoring mistake.
	list, err := Parse([]string{"good.example.com", "", "  "})
	if err != nil {
		t.Fatalf("Parse: unexpected error %v", err)
	}
	if got := list.Entries(); len(got) != 1 || got[0] != "good.example.com" {
		t.Fatalf("Parse entries = %v, want [good.example.com]", got)
	}
}

func TestParsePartialKeepsTheGoodEntries(t *testing.T) {
	t.Parallel()

	list, bad := ParsePartial([]string{"good.example.com", "https://bad", "10.0.0.0/8"})
	if len(bad) != 1 || bad[0] != "https://bad" {
		t.Fatalf("ParsePartial bad = %v, want [https://bad]", bad)
	}
	got := list.Entries()
	if len(got) != 2 {
		t.Fatalf("ParsePartial entries = %v, want two", got)
	}
}

func TestEntriesAreDeduplicatedAndSorted(t *testing.T) {
	t.Parallel()

	list, err := Parse([]string{"b.example.com", "A.example.com", "a.example.com.", "b.example.com"})
	if err != nil {
		t.Fatalf("Parse: unexpected error %v", err)
	}
	got := strings.Join(list.Entries(), ",")
	if got != "a.example.com,b.example.com" {
		t.Fatalf("Entries() = %q, want %q", got, "a.example.com,b.example.com")
	}
}

func TestAllowsMatchesTheDocumentedGrammar(t *testing.T) {
	t.Parallel()

	list, err := Parse([]string{
		"api.openai.com",
		"pinned.example.com:8443",
		"*.openai.azure.com",
		"*.ports.example.com:9000",
		"192.168.29.60:8000",
		"10.20.0.0/16",
	})
	if err != nil {
		t.Fatalf("Parse: unexpected error %v", err)
	}

	allowed := []string{
		"https://api.openai.com/v1",
		"https://API.OpenAI.com/v1",
		"https://api.openai.com:443/v1",
		"http://api.openai.com:80/v1",
		"https://pinned.example.com:8443/v1",
		"https://eastus.openai.azure.com/openai",
		"https://a.b.openai.azure.com/openai",
		"https://x.ports.example.com:9000/v1",
		"http://192.168.29.60:8000/v1",
		"http://10.20.5.7:11434/v1",
		// An empty api_base means "use the provider's own default", which is not
		// tenant-controlled and is not what this allowlist governs.
		"",
		"   ",
	}
	for _, base := range allowed {
		if !list.Allows(base) {
			t.Errorf("Allows(%q) = false, want true", base)
		}
	}

	refused := []string{
		"https://evil.com/v1",
		// A pinned port does not match another port.
		"https://pinned.example.com/v1",
		"https://pinned.example.com:9443/v1",
		// A wildcard never matches the bare domain.
		"https://openai.azure.com/openai",
		// A wildcard with a pinned port does not match another port.
		"https://x.ports.example.com/v1",
		// The exact IP entry pinned port 8000.
		"http://192.168.29.60:9000/v1",
		// Outside the CIDR.
		"http://10.21.0.1:8000/v1",
		// Unparsable values fail CLOSED: this is what a parser-differential
		// probe looks like.
		"://",
		"http://",
		"not a url at all",
	}
	for _, base := range refused {
		if list.Allows(base) {
			t.Errorf("Allows(%q) = true, want false", base)
		}
	}
}

func TestCIDRNeverMatchesAName(t *testing.T) {
	t.Parallel()

	// The whole reason this is a NAME allowlist: resolving a name here and
	// dialing it later is the DNS-rebinding race. A CIDR entry therefore
	// matches IP literals only, and an operator who runs a private endpoint
	// under a name must also name it.
	list, err := Parse([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("Parse: unexpected error %v", err)
	}
	if list.Allows("http://vllm.ml.svc.cluster.local:8000/v1") {
		t.Fatal("a CIDR entry matched a hostname; it must match IP literals only")
	}
	if !list.Allows("http://10.1.2.3:8000/v1") {
		t.Fatal("a CIDR entry did not match an IP literal inside it")
	}
}

func TestUnconfiguredAllowlistAllowsEveryHostButNoPrivateNetwork(t *testing.T) {
	t.Parallel()

	list, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse: unexpected error %v", err)
	}
	if list.Configured() {
		t.Fatal("an empty allowlist reported itself configured")
	}
	if !list.Allows("https://anything.example.com/v1") {
		t.Fatal("an unconfigured allowlist must impose no host restriction")
	}
	// The safety of the line above depends entirely on this one: with no entry
	// the SSRF-safe dialer stays on, so no tenant reaches a private address.
	if list.AllowsPrivateNetwork() {
		t.Fatal("an unconfigured allowlist must not unlock the private network")
	}
	var nilList *Allowlist
	if nilList.Configured() || nilList.AllowsPrivateNetwork() {
		t.Fatal("a nil allowlist must be inert, not permissive about the private network")
	}
	if len(nilList.Entries()) != 0 {
		t.Fatal("a nil allowlist must report no entries")
	}
}

func TestAllowsPrivateNetworkNeedsAnExplicitPrivateEntry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		entries []string
		want    bool
	}{
		{"public hosts only", []string{"api.openai.com", "*.openai.azure.com"}, false},
		{"a public CIDR", []string{"52.0.0.0/8"}, false},
		// Link-local is refused by the dialer whatever the flag says, so naming
		// it grants nothing and must not claim to.
		{"link-local names nothing grantable", []string{"169.254.169.254"}, false},
		{"an RFC 1918 literal", []string{"192.168.29.60:8000"}, true},
		{"an RFC 1918 block", []string{"10.0.0.0/8"}, true},
		{"a block that overlaps one", []string{"192.168.29.0/24"}, true},
		{"a supernet that overlaps one", []string{"0.0.0.0/0"}, true},
		{"loopback", []string{"127.0.0.1:11434"}, true},
		{"localhost by name", []string{"localhost:11434"}, true},
		{"IPv6 unique-local", []string{"fd00::/8"}, true},
		// A wildcard names a DNS subtree and says nothing about where it points.
		{"a wildcard", []string{"*.cluster.local"}, false},
		{"one private entry among public ones", []string{"api.openai.com", "10.0.0.0/8"}, true},
	}
	for _, tc := range cases {
		list, err := Parse(tc.entries)
		if err != nil {
			t.Fatalf("%s: Parse: unexpected error %v", tc.name, err)
		}
		if got := list.AllowsPrivateNetwork(); got != tc.want {
			t.Errorf("%s: AllowsPrivateNetwork() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMergeIsAUnionAndCannotWithdrawTheFloor(t *testing.T) {
	t.Parallel()

	floor, err := Parse([]string{"api.openai.com"})
	if err != nil {
		t.Fatalf("Parse: unexpected error %v", err)
	}
	merged, bad := floor.Merge([]string{"192.168.29.60:8000", "https://malformed"})

	if len(bad) != 1 || bad[0] != "https://malformed" {
		t.Fatalf("Merge bad = %v, want [https://malformed]", bad)
	}
	// The environment floor survives an authored set that does not mention it.
	if !merged.Allows("https://api.openai.com/v1") {
		t.Fatal("the merged allowlist dropped an environment entry; the floor must be a union member")
	}
	if !merged.Allows("http://192.168.29.60:8000/v1") {
		t.Fatal("the merged allowlist did not gain the authored entry")
	}
	if !merged.AllowsPrivateNetwork() {
		t.Fatal("an authored private entry must unlock the private network")
	}
	// The floor itself is unchanged: Merge returns a new value.
	if floor.AllowsPrivateNetwork() {
		t.Fatal("Merge mutated the receiver")
	}
	if len(floor.Entries()) != 1 {
		t.Fatalf("Merge mutated the receiver's entries: %v", floor.Entries())
	}
}
