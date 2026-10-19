package runtoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAllowlistGrammar(t *testing.T) {
	good := map[string]string{
		"example.com":           "example.com",
		"Example.COM.":          "example.com",
		"example.com:8443":      "example.com:8443",
		"*.example.com":         "*.example.com",
		"*.example.com:8443":    "*.example.com:8443",
		"api_v2.example.com":    "api_v2.example.com",
		"203.0.113.7":           "203.0.113.7",
		"203.0.113.7:8443":      "203.0.113.7:8443",
		"2606:4700::1111":       "[2606:4700::1111]",
		"[2606:4700::1111]":     "[2606:4700::1111]",
		"[2606:4700::1111]:22":  "[2606:4700::1111]:22",
		"::ffff:203.0.113.7":    "203.0.113.7",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	}
	for in, want := range good {
		r, err := ParseRule(in)
		if err != nil {
			t.Errorf("ParseRule(%q): %v", in, err)
			continue
		}
		if r.String() != want {
			t.Errorf("ParseRule(%q) = %q, want %q", in, r.String(), want)
		}
		// The normalised spelling parses back to itself.
		if again, err := ParseRule(r.String()); err != nil || again.String() != want {
			t.Errorf("round trip of %q: %v %q", want, err, again.String())
		}
	}
	bad := []string{
		"", "   ", "*", "*.", "*.com", "*.*.example.com", "a.*.example.com", "ex*ample.com",
		"https://example.com", "example.com/path", "10.0.0.0/8", "user@example.com",
		"example.com:0", "example.com:65536", "example.com:+443", "example.com:0443", "example.com:http",
		"example.com:", "exa mple.com", "-example.com", "example-.com", "exa..mple.com",
		"bücher.example", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 130) + "com",
		// IP literals that can never be allowed.
		"10.0.0.1", "127.0.0.1", "192.168.1.1:443", "169.254.169.254", "[::1]:443", "fc00::1",
		"100.64.0.1", "0.0.0.0", "100.100.100.200", "::ffff:10.0.0.1", "64:ff9b::a9fe:a9fe",
		// Names some resolvers read as IPv4.
		"1.2.3", "127.1", "0x7f.1", "2130706433",
		// IPv6 with a port needs brackets; brackets need IPv6.
		"[example.com]:443", "[10.0.0.1]", "[203.0.113.7]", "[203.0.113.7]:443",
	}
	for _, in := range bad {
		if r, err := ParseRule(in); err == nil {
			t.Errorf("ParseRule(%q) accepted %q, want a refusal", in, r.String())
		}
	}
	if _, err := ParseAllowlist(make([]string, MaxEntries+1)); err == nil {
		t.Error("an allowlist over MaxEntries must be refused")
	}
	if _, err := ParseAllowlist([]string{"example.com", ""}); err == nil {
		t.Error("a blank entry must be refused")
	}
}

func TestAllowlistMatching(t *testing.T) {
	a, err := ParseAllowlist([]string{"api.example.com", "*.cdn.example.net", "pinned.example.org:8443", "203.0.113.7", "[2606:4700::1111]:8443", "api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Len() != 5 {
		t.Errorf("duplicates must collapse: %v", a.Entries())
	}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"api.example.com", 443, true},
		{"API.Example.com.", 80, true},
		{"api.example.com", 8443, false}, // no port: only 443 and 80
		{"api.example.com", 22, false},
		{"other.example.com", 443, false},
		{"example.com", 443, false},
		{"evilapi.example.com", 443, false},
		{"api.example.com.evil.test", 443, false},
		{"a.cdn.example.net", 443, true},
		{"a.b.cdn.example.net", 80, true},
		{"cdn.example.net", 443, false}, // a wildcard never matches its own suffix
		{"xcdn.example.net", 443, false},
		{"pinned.example.org", 8443, true},
		{"pinned.example.org", 443, false},
		{"203.0.113.7", 443, true},
		{"::ffff:203.0.113.7", 443, true},
		{"203.0.113.8", 443, false},
		{"[2606:4700::1111]", 8443, true},
		{"2606:4700::1111", 443, false},
		// An IP literal never matches a name rule, and a name never an IP rule.
		{"93.184.216.34", 443, false},
	}
	for _, tc := range cases {
		target, err := ParseTarget(tc.host, tc.port)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tc.host, err)
			continue
		}
		if got := a.Allows(target); got != tc.want {
			t.Errorf("Allows(%s:%d) = %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}
	for host, want := range map[string]bool{"api.example.com": true, "pinned.example.org": true, "x.cdn.example.net": true, "other.example": false} {
		target, _ := ParseTarget(host, 22)
		if a.Allows(target) || a.AllowsHost(target) != want {
			t.Errorf("AllowsHost(%s:22): want Allows false and AllowsHost %v", host, want)
		}
	}
	for _, bad := range []string{"exa mple.com", "fe80::1%eth0", "a/b", ""} {
		if _, err := ParseTarget(bad, 443); err == nil {
			t.Errorf("ParseTarget(%q) must refuse", bad)
		}
	}
	if _, err := ParseTarget("example.com", 0); err == nil {
		t.Error("port 0 must be refused")
	}
	var nilList *Allowlist
	if nilList.Allows(Target{Host: "example.com", Port: 443}) {
		t.Error("a nil allowlist allows nothing")
	}
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

var t0 = time.Unix(1_800_000_000, 0)

func claims(t *testing.T) Claims {
	t.Helper()
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return Claims{
		ExecutionID: "exec-0f3a:7",
		ProjectID:   42,
		Allowlist:   []string{"api.example.com", "*.pypi.org"},
		IssuedAt:    t0,
		NotBefore:   t0,
		ExpiresAt:   t0.Add(10 * time.Minute),
		Nonce:       nonce,
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv := newKey(t)
	signer, err := NewSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := NewKeySet(pub)
	if err != nil {
		t.Fatal(err)
	}
	c := claims(t)
	tok, err := signer.Sign(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, Prefix+"."+signer.KeyID()+".") {
		t.Fatalf("token shape: %s", tok)
	}
	for _, r := range tok {
		ok := r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			t.Fatalf("token holds %q, which a proxy URL password must escape", r)
		}
	}
	v, err := ks.Verify(tok, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if v.Claims.ExecutionID != c.ExecutionID || v.Claims.ProjectID != 42 || v.Claims.KeyID != signer.KeyID() ||
		v.Claims.Nonce != c.Nonce || !v.Claims.ExpiresAt.Equal(c.ExpiresAt) || v.Allowlist.Len() != 2 {
		t.Fatalf("claims did not round-trip: %+v", v.Claims)
	}
	target, _ := ParseTarget("files.pypi.org", 443)
	if !v.Allowlist.Allows(target) {
		t.Fatal("the verified allowlist must admit its own entries")
	}
}

func TestVerifyRefusals(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	signer, _ := NewSigner(priv)
	otherSigner, _ := NewSigner(otherPriv)
	ks, _ := NewKeySet(pub)
	good, err := signer.Sign(claims(t))
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Minute)

	expect := func(name, tok string, at time.Time, want error) {
		t.Helper()
		_, err := ks.Verify(tok, at)
		if !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", name, err, want)
		}
		if err != nil && strings.Contains(err.Error(), tok) && tok != "" {
			t.Errorf("%s: the error echoes the token", name)
		}
	}

	expect("expired", good, t0.Add(10*time.Minute), ErrExpired)
	expect("long expired", good, t0.Add(48*time.Hour), ErrExpired)
	expect("not yet valid", good, t0.Add(-time.Minute), ErrNotYetValid)
	if _, err := ks.Verify(good, t0.Add(-ClockSkew+time.Second)); err != nil {
		t.Errorf("a not-before inside the clock skew must pass: %v", err)
	}

	fromOther, _ := otherSigner.Sign(claims(t))
	expect("unknown kid", fromOther, now, ErrUnknownKey)

	// The other key's signature under this key's kid: a wrong key.
	parts := strings.Split(good, ".")
	otherParts := strings.Split(fromOther, ".")
	expect("wrong key", strings.Join([]string{parts[0], parts[1], parts[2], otherParts[3]}, "."), now, ErrBadSignature)

	// Tampering with the payload: widen the allowlist.
	var w wireClaims
	raw, _ := b64.DecodeString(parts[2])
	_ = json.Unmarshal(raw, &w)
	w.Allowlist = append(w.Allowlist, "evil.example")
	tampered, _ := json.Marshal(w)
	expect("tampered payload", strings.Join([]string{parts[0], parts[1], b64.EncodeToString(tampered), parts[3]}, "."), now, ErrBadSignature)

	// Tampering with the kid of a set member: the signature covers it.
	ks2, _ := NewKeySet(pub, otherPub)
	if _, err := ks2.Verify(strings.Join([]string{parts[0], KeyID(otherPub), parts[2], parts[3]}, "."), now); !errors.Is(err, ErrBadSignature) {
		t.Errorf("swapped kid: got %v, want ErrBadSignature", err)
	}

	sig, _ := b64.DecodeString(parts[3])
	sig[0] ^= 1
	expect("flipped signature", strings.Join([]string{parts[0], parts[1], parts[2], b64.EncodeToString(sig)}, "."), now, ErrBadSignature)

	expect("too large", good+strings.Repeat("A", MaxTokenBytes), now, ErrTooLarge)
	expect("empty", "", now, ErrMalformed)
	expect("three parts", strings.Join(parts[:3], "."), now, ErrMalformed)
	expect("wrong version", "ert2"+good[4:], now, ErrMalformed)
	expect("bad base64 signature", strings.Join([]string{parts[0], parts[1], parts[2], "!!!"}, "."), now, ErrMalformed)

	var nilSet *KeySet
	if _, err := nilSet.Verify(good, now); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("a nil key set must refuse: %v", err)
	}
}

// signRaw signs arbitrary wire claims, to prove Verify re-checks what Sign
// would refuse.
func signRaw(t *testing.T, priv ed25519.PrivateKey, w wireClaims) string {
	t.Helper()
	payload, _ := json.Marshal(w)
	kid := KeyID(priv.Public().(ed25519.PublicKey))
	signed := Prefix + "." + kid + "." + b64.EncodeToString(payload)
	return signed + "." + b64.EncodeToString(ed25519.Sign(priv, []byte(signed)))
}

func TestVerifyRechecksSignedClaims(t *testing.T) {
	pub, priv := newKey(t)
	ks, _ := NewKeySet(pub)
	kid := KeyID(pub)
	base := wireClaims{Version: 1, KeyID: kid, ExecutionID: "e1", ProjectID: 1, Allowlist: []string{"example.com"},
		IssuedAt: t0.Unix(), NotBefore: t0.Unix(), ExpiresAt: t0.Add(time.Hour).Unix(), Nonce: strings.Repeat("A", 22)}
	now := t0.Add(time.Minute)
	if _, err := ks.Verify(signRaw(t, priv, base), now); err != nil {
		t.Fatalf("base claims must pass: %v", err)
	}
	mutate := map[string]func(w *wireClaims){
		"private allowlist entry": func(w *wireClaims) { w.Allowlist = []string{"10.0.0.1"} },
		"cidr entry":              func(w *wireClaims) { w.Allowlist = []string{"0.0.0.0/0"} },
		"lifetime over max":       func(w *wireClaims) { w.ExpiresAt = t0.Add(MaxLifetime + time.Hour).Unix() },
		"expiry before nbf":       func(w *wireClaims) { w.ExpiresAt = t0.Add(-time.Hour).Unix() },
		"kid mismatch":            func(w *wireClaims) { w.KeyID = "0000000000000000" },
		"no execution":            func(w *wireClaims) { w.ExecutionID = "" },
		"log-unsafe execution":    func(w *wireClaims) { w.ExecutionID = "e1\nlevel=error" },
		"zero project":            func(w *wireClaims) { w.ProjectID = 0 },
		"short nonce":             func(w *wireClaims) { w.Nonce = "abc" },
		"no issue time":           func(w *wireClaims) { w.IssuedAt = 0 },
	}
	for name, m := range mutate {
		w := base
		m(&w)
		if _, err := ks.Verify(signRaw(t, priv, w), now); !errors.Is(err, ErrInvalidClaims) {
			t.Errorf("%s: got %v, want ErrInvalidClaims", name, err)
		}
	}
	w := base
	w.Version = 2
	if _, err := ks.Verify(signRaw(t, priv, w), now); !errors.Is(err, ErrMalformed) {
		t.Errorf("version 2 in a v1 envelope: got %v", err)
	}
	// An unknown field is refused, so a v1 verifier never ignores a claim.
	payload := []byte(`{"v":1,"kid":"` + kid + `","eid":"e1","pid":1,"allow":[],"iat":1,"nbf":1,"exp":2,"nonce":"AAAAAAAAAAAAAAAAAAAAAA","extra":1}`)
	signed := Prefix + "." + kid + "." + b64.EncodeToString(payload)
	tok := signed + "." + b64.EncodeToString(ed25519.Sign(priv, []byte(signed)))
	if _, err := ks.Verify(tok, time.Unix(1, 0)); !errors.Is(err, ErrMalformed) {
		t.Errorf("unknown field: got %v", err)
	}
}

func TestSignRefusesBadClaims(t *testing.T) {
	_, priv := newKey(t)
	signer, _ := NewSigner(priv)
	c := claims(t)
	c.Allowlist = []string{"192.168.0.1"}
	if _, err := signer.Sign(c); !errors.Is(err, ErrInvalidClaims) {
		t.Errorf("private entry: %v", err)
	}
	c = claims(t)
	c.ExpiresAt = c.NotBefore.Add(MaxLifetime + time.Second)
	if _, err := signer.Sign(c); !errors.Is(err, ErrInvalidClaims) {
		t.Errorf("long lifetime: %v", err)
	}
	c = claims(t)
	c.Allowlist = nil
	for i := 0; i < MaxEntries; i++ {
		c.Allowlist = append(c.Allowlist, strings.Repeat("a", 55)+"-"+string(rune('a'+i%26))+strings.Repeat("b", i/26)+".example.com")
	}
	if _, err := signer.Sign(c); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a token over MaxTokenBytes must be refused at signing: %v", err)
	}
	if _, err := NewSigner(ed25519.PrivateKey{1, 2, 3}); err == nil {
		t.Error("a short key must be refused")
	}
}

func TestKeysPEMAndRotation(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, newPriv := newKey(t)
	dir := t.TempDir()
	privPEM, err := MarshalPrivateKeyPEM(newPriv)
	if err != nil {
		t.Fatal(err)
	}
	privPath := filepath.Join(dir, "signing.pem")
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPrivateKeyFile(privPath)
	if err != nil || !loaded.Equal(newPriv) {
		t.Fatalf("private key round trip: %v", err)
	}

	keyDir := filepath.Join(dir, "keys")
	_ = os.Mkdir(keyDir, 0o700)
	oldPEM, _ := MarshalPublicKeyPEM(oldPub)
	newPEM, _ := MarshalPublicKeyPEM(newPub)
	_ = os.WriteFile(filepath.Join(keyDir, "old.pem"), oldPEM, 0o600)
	_ = os.WriteFile(filepath.Join(keyDir, "new.pem"), newPEM, 0o600)
	_ = os.WriteFile(filepath.Join(keyDir, "..data"), []byte("not a key"), 0o600)
	ks, err := LoadKeySet(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ks.KeyIDs()) != 2 {
		t.Fatalf("want both keys during the rotation, got %v", ks.KeyIDs())
	}
	oldSigner, _ := NewSigner(oldPriv)
	newSigner, _ := NewSigner(newPriv)
	for _, s := range []*Signer{oldSigner, newSigner} {
		tok, _ := s.Sign(claims(t))
		if _, err := ks.Verify(tok, t0); err != nil {
			t.Errorf("key %s: %v", s.KeyID(), err)
		}
	}

	// One file holding both keys is the same set.
	both := filepath.Join(dir, "both.pem")
	_ = os.WriteFile(both, append(append([]byte("# old\n"), oldPEM...), newPEM...), 0o600)
	if ks, err := LoadKeySet(both); err != nil || len(ks.KeyIDs()) != 2 {
		t.Fatalf("one file, two keys: %v", err)
	}

	if _, err := ParsePublicKeysPEM(privPEM); err == nil {
		t.Error("a private key in the public set must be refused")
	}
	if _, err := ParsePublicKeysPEM([]byte("nothing")); err == nil {
		t.Error("no PEM block must be refused")
	}
	if _, err := LoadKeySet(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing path must be refused")
	}
	if _, err := NewKeySet(); err == nil {
		t.Error("an empty set must be refused")
	}
	big := filepath.Join(dir, "big.pem")
	_ = os.WriteFile(big, make([]byte, maxKeyFileBytes+1), 0o600)
	if _, err := LoadKeySet(big); err == nil {
		t.Error("an oversize key file must be refused")
	}
}
