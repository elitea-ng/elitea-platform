package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureClientHello runs a crypto/tls client handshake against a pipe and
// returns the bytes of its first flight: the records of one ClientHello.
func captureClientHello(t testing.TB, cfg *tls.Config) []byte {
	t.Helper()
	c, s := net.Pipe()
	defer func() { _ = c.Close(); _ = s.Close() }()
	go func() { _ = tls.Client(c, cfg).Handshake() }()
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		n, err := s.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if _, complete, perr := scanClientHello(buf, 1<<20); complete || perr != nil {
			if perr != nil {
				t.Fatalf("crypto/tls ClientHello did not parse: %v", perr)
			}
			return buf
		}
		if err != nil {
			t.Fatalf("capture ClientHello: %v", err)
		}
	}
}

func u16(n int) []byte { return []byte{byte(n >> 8), byte(n)} }

// ext encodes one extension.
func ext(typ uint16, data []byte) []byte {
	out := binary.BigEndian.AppendUint16(nil, typ)
	out = append(out, u16(len(data))...)
	return append(out, data...)
}

// sniExt encodes a server_name extension with one host_name.
func sniExt(name string) []byte {
	entry := append([]byte{serverNameHostName}, u16(len(name))...)
	entry = append(entry, name...)
	return ext(extensionServerName, append(u16(len(entry)), entry...))
}

// helloBody builds a ClientHello body with the given extensions; nil means
// no extensions block at all.
func helloBody(exts ...[]byte) []byte {
	b := []byte{0x03, 0x03}
	b = append(b, make([]byte, 32)...)    // random
	b = append(b, 0)                      // session id
	b = append(b, 0x00, 0x02, 0x13, 0x01) // one suite
	b = append(b, 0x01, 0x00)             // null compression
	if exts == nil {
		return b
	}
	var all []byte
	for _, e := range exts {
		all = append(all, e...)
	}
	b = append(b, u16(len(all))...)
	return append(b, all...)
}

// handshake wraps a ClientHello body in its handshake header.
func handshake(body []byte) []byte {
	n := len(body)
	return append([]byte{handshakeTypeClientHello, byte(n >> 16), byte(n >> 8), byte(n)}, body...)
}

// records splits handshake bytes into handshake records with fragments of
// the given sizes; the last fragment takes the rest.
func records(hs []byte, sizes ...int) []byte {
	var out []byte
	for _, n := range sizes {
		if n >= len(hs) {
			break
		}
		out = append(out, recordTypeHandshake, 0x03, 0x01)
		out = append(out, u16(n)...)
		out = append(out, hs[:n]...)
		hs = hs[n:]
	}
	out = append(out, recordTypeHandshake, 0x03, 0x01)
	out = append(out, u16(len(hs))...)
	return append(out, hs...)
}

func helloRecords(exts ...[]byte) []byte { return records(handshake(helloBody(exts...))) }

func TestScanClientHello(t *testing.T) {
	real := captureClientHello(t, &tls.Config{ServerName: "Example.COM", MinVersion: tls.VersionTLS12})
	noSNI := captureClientHello(t, &tls.Config{ServerName: "203.0.113.10", InsecureSkipVerify: true})
	ech := helloRecords(sniExt("public.example"), ext(extensionECH, []byte{0, 1, 2, 3}))
	hs := handshake(helloBody(sniExt("example.com")))

	tests := []struct {
		name     string
		in       []byte
		max      int
		complete bool
		err      error
		want     clientHello
	}{
		{name: "crypto/tls", in: real, complete: true, want: clientHello{serverName: "Example.COM", hasServerName: true}},
		{name: "crypto/tls to an IP has no SNI", in: noSNI, complete: true},
		{name: "ECH outer SNI", in: ech, complete: true, want: clientHello{serverName: "public.example", hasServerName: true, hasECH: true}},
		{name: "no extensions block", in: helloRecords(), complete: true},
		{name: "empty extensions", in: helloRecords([]byte{}), complete: true},
		{name: "two records", in: records(hs, 10), complete: true, want: clientHello{serverName: "example.com", hasServerName: true}},
		{name: "header split over records", in: records(hs, 1, 2, 3), complete: true, want: clientHello{serverName: "example.com", hasServerName: true}},
		{name: "trailing bytes after the hello are ignored", in: append(helloRecords(sniExt("a.example")), 0x17, 0x03, 0x03), complete: true, want: clientHello{serverName: "a.example", hasServerName: true}},
		{name: "empty", in: nil},
		{name: "one byte", in: []byte{0x16}},
		{name: "partial header", in: []byte{0x16, 0x03, 0x01, 0x00}},
		{name: "partial record", in: helloRecords(sniExt("example.com"))[:20]},
		{name: "first record of two", in: records(hs, 10)[:15]},
		{name: "plain http", in: []byte("GET / HTTP/1.1\r\n"), err: errNotTLS},
		{name: "SSLv2 hello", in: []byte{0x80, 0x2e, 0x01}, err: errNotTLS},
		{name: "bad record version", in: []byte{0x16, 0x07, 0x01, 0x00, 0x05}, err: errHelloMalformed},
		{name: "zero-length record", in: []byte{0x16, 0x03, 0x01, 0x00, 0x00}, err: errHelloMalformed},
		{name: "record over 2^14", in: []byte{0x16, 0x03, 0x01, 0x40, 0x01}, err: errHelloMalformed},
		{name: "not a ClientHello", in: records(append([]byte{0x02}, hs[1:]...)), err: errHelloMalformed},
		{name: "alert between fragments", in: append(records(hs, 10)[:15], 0x15, 0x03, 0x03, 0x00, 0x02), err: errHelloMalformed},
		{name: "duplicate SNI", in: helloRecords(sniExt("a.example"), sniExt("b.example")), err: errHelloMalformed},
		{name: "two names in one SNI", in: helloRecords(ext(extensionServerName, []byte{0, 8, 0, 0, 1, 'a', 0, 0, 1, 'b'})), err: errHelloMalformed},
		{name: "empty SNI name", in: helloRecords(ext(extensionServerName, []byte{0, 3, 0, 0, 0})), err: errHelloMalformed},
		{name: "unknown name type", in: helloRecords(ext(extensionServerName, []byte{0, 4, 1, 0, 1, 'a'})), err: errHelloMalformed},
		{name: "SNI list overruns", in: helloRecords(ext(extensionServerName, []byte{0, 9, 0, 0, 1, 'a'})), err: errHelloMalformed},
		{name: "extension overruns", in: records(handshake(append(helloBody(), 0x00, 0x08, 0x00, 0x00, 0x00, 0x09))), err: errHelloMalformed},
		{name: "bytes after extensions", in: records(handshake(append(helloBody([]byte{}), 0xff))), err: errHelloMalformed},
		{name: "truncated body", in: records(handshake(helloBody()[:20])), err: errHelloMalformed},
		{name: "declared hello over the bound", in: records([]byte{0x01, 0x00, 0x40, 0x00, 0x03}), max: 4096, err: errHelloTooLarge},
		{name: "record over the bound", in: []byte{0x16, 0x03, 0x01, 0x10, 0x00}, max: 1024, err: errHelloTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limit := tc.max
			if limit == 0 {
				limit = ClientHelloMaxBytes
			}
			got, complete, err := scanClientHello(tc.in, limit)
			if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if complete != tc.complete {
				t.Fatalf("complete = %v, want %v", complete, tc.complete)
			}
			if got != tc.want {
				t.Fatalf("hello = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestScanClientHelloByteByByte feeds a real ClientHello one byte at a time:
// every prefix needs more, the whole parses.
func TestScanClientHelloByteByByte(t *testing.T) {
	full := records(handshake(helloBody(sniExt("example.com"))), 7, 30)
	for i := 0; i < len(full); i++ {
		if _, complete, err := scanClientHello(full[:i], ClientHelloMaxBytes); complete || err != nil {
			t.Fatalf("prefix %d: complete=%v err=%v", i, complete, err)
		}
	}
	if h, complete, err := scanClientHello(full, ClientHelloMaxBytes); !complete || err != nil || h.serverName != "example.com" {
		t.Fatalf("whole: %+v %v %v", h, complete, err)
	}
}

func TestSameHost(t *testing.T) {
	for _, tc := range []struct {
		sni, host string
		want      bool
	}{
		{"example.com", "example.com", true},
		{"EXAMPLE.com", "example.com", true},
		{"example.com.", "example.com", true},
		{"example.com", "example.com.", true},
		{"example.com..", "example.com", false},
		{"other.example", "example.com", false},
		{"sub.example.com", "example.com", false},
		{"", "example.com", false},
		{".", "", false},
		{"Key.example", "key.example", false}, // the Kelvin sign folds to k in Unicode, not here
		{"exampl\x00.com", "example.com", false},
	} {
		if got := sameHost(tc.sni, tc.host); got != tc.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", tc.sni, tc.host, got, tc.want)
		}
	}
}

func FuzzParseClientHello(f *testing.F) {
	f.Add(captureClientHello(f, &tls.Config{ServerName: "example.com", MinVersion: tls.VersionTLS12}))
	f.Add(captureClientHello(f, &tls.Config{ServerName: "203.0.113.10", InsecureSkipVerify: true}))
	f.Add(helloRecords(sniExt("public.example"), ext(extensionECH, []byte{0, 1, 2, 3})))
	f.Add(records(handshake(helloBody(sniExt("example.com"))), 1, 2, 3))
	f.Add(helloRecords())
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		h, complete, err := scanClientHello(data, ClientHelloMaxBytes)
		if err != nil && complete {
			t.Fatal("an error with a complete hello")
		}
		if !complete && h != (clientHello{}) {
			t.Fatal("a hello without completion")
		}
		if complete {
			// A complete hello stays complete, with the same result, when
			// more bytes follow it.
			h2, complete2, err2 := scanClientHello(append(append([]byte(nil), data...), 0x17), ClientHelloMaxBytes)
			if !complete2 || err2 != nil || h2 != h {
				t.Fatalf("appending a byte changed the result: %+v %v %v", h2, complete2, err2)
			}
		}
		// The body parser on its own, too.
		_, _ = parseClientHello(data)
		_, _ = parseServerName(data)
	})
}

// recordingDestination is a TCP destination that keeps every byte each
// connection sends, and closes once the client half-closes.
type recordingDestination struct {
	mu       sync.Mutex
	accepted int
	received [][]byte
	addr     string
}

func newRecordingDestination(t *testing.T) *recordingDestination {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	d := &recordingDestination{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			d.accepted++
			i := len(d.received)
			d.received = append(d.received, nil)
			d.mu.Unlock()
			go func() {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					d.mu.Lock()
					d.received[i] = append(d.received[i], buf[:n]...)
					d.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return d
}

func (d *recordingDestination) snapshot() (int, [][]byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]byte, len(d.received))
	for i, b := range d.received {
		out[i] = append([]byte(nil), b...)
	}
	return d.accepted, out
}

// sniHarness routes front.example (203.0.113.50) on 443 and 8443, and the
// IP literal 203.0.113.51 on 443, to one recording destination.
func sniHarness(t *testing.T, opts ...harnessOption) (*harness, *recordingDestination, string) {
	t.Helper()
	h := newHarness(t, opts...)
	dest := newRecordingDestination(t)
	h.resolver.set("front.example", []string{"203.0.113.50"})
	h.dialer.route("203.0.113.50:443", dest.addr)
	h.dialer.route("203.0.113.50:8443", dest.addr)
	h.dialer.route("203.0.113.51:443", dest.addr)
	auth := basic(h.token("exec-sni", []string{"front.example", "front.example:8443", "203.0.113.51"}, time.Hour))
	return h, dest, auth
}

// sendThrough opens a tunnel, writes the chunks (pausing between them), and
// returns what the client then reads until the tunnel closes.
func sendThrough(t *testing.T, h *harness, authority, auth string, chunks ...[]byte) []byte {
	t.Helper()
	resp, c, br := h.rawConnect(authority, auth)
	defer func() { _ = c.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT %s: %d %q", authority, resp.StatusCode, resp.Header.Get(RefusalHeader))
	}
	for i, chunk := range chunks {
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := c.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := io.ReadAll(br)
	return got
}

// What a refused client reads back after the 200.
type alertWant int

const (
	alertSent   alertWant = iota // exactly the access_denied alert
	alertNone                    // nothing: the tunnel just closes
	alertEither                  // the alert, or nothing when the proxy closed with unread client bytes (a reset may drop it)
)

func expectSNIRefusal(t *testing.T, h *harness, dest *recordingDestination, reason string, back []byte, want alertWant) {
	t.Helper()
	switch {
	case want == alertSent && !bytes.Equal(back, alertAccessDenied),
		want == alertNone && len(back) != 0,
		want == alertEither && len(back) != 0 && !bytes.Equal(back, alertAccessDenied):
		t.Errorf("the client read %x after the refusal", back)
	}
	waitFor(t, 2*time.Second, "the refusal log line", func() bool {
		return strings.Contains(h.logs.String(), `"reason":"`+reason+`"`)
	})
	logs := h.logs.String()
	if !strings.Contains(logs, `"outcome":"refused"`) || !strings.Contains(logs, `"msg":"egress_proxy.connection"`) {
		t.Errorf("the refusal log line is not the connection shape:\n%s", logs)
	}
	if n := h.metrics.RefusalCount(reason); n != 1 {
		t.Errorf("%s counted %d, want 1", reason, n)
	}
	if d := h.dialer.dialedAddrs(); len(d) != 0 {
		t.Errorf("a refused tunnel dialled %v", d)
	}
	if accepted, _ := dest.snapshot(); accepted != 0 {
		t.Errorf("the destination saw %d connections", accepted)
	}
}

func TestConnectSNIPasses(t *testing.T) {
	cases := []struct {
		name      string
		authority string
		chunks    func([]byte) [][]byte
		hello     []byte
	}{
		{name: "exact", authority: "front.example:443", hello: helloRecords(sniExt("front.example"))},
		{name: "case", authority: "FRONT.Example:443", hello: helloRecords(sniExt("Front.EXAMPLE"))},
		{name: "SNI trailing dot", authority: "front.example:443", hello: helloRecords(sniExt("front.example."))},
		{name: "CONNECT trailing dot", authority: "front.example.:443", hello: helloRecords(sniExt("front.example"))},
		{name: "no SNI to an IP literal", authority: "203.0.113.51:443", hello: helloRecords()},
		{name: "crypto/tls to an IP literal", authority: "203.0.113.51:443", hello: captureClientHello(t, &tls.Config{ServerName: "203.0.113.51", InsecureSkipVerify: true})},
		{name: "crypto/tls", authority: "front.example:443", hello: captureClientHello(t, &tls.Config{ServerName: "front.example", MinVersion: tls.VersionTLS12})},
		{name: "ECH public name is the CONNECT host", authority: "front.example:443", hello: helloRecords(sniExt("front.example"), ext(extensionECH, []byte{0, 1, 2}))},
		{name: "fragmented records", authority: "front.example:443", hello: records(handshake(helloBody(sniExt("front.example"))), 3, 1, 40)},
		{name: "TLS on another port", authority: "front.example:8443", hello: helloRecords(sniExt("front.example"))},
		{name: "plain bytes on another port", authority: "front.example:8443", hello: []byte("SSH-2.0-not-tls\r\n")},
		{
			name: "fragmented TCP writes", authority: "front.example:443", hello: records(handshake(helloBody(sniExt("front.example"))), 30),
			chunks: func(b []byte) [][]byte { return [][]byte{b[:1], b[1:4], b[4:20], b[20:]} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, dest, auth := sniHarness(t)
			payload := append(append([]byte(nil), tc.hello...), "after the hello"...)
			chunks := [][]byte{payload}
			if tc.chunks != nil {
				chunks = tc.chunks(tc.hello)
				chunks = append(chunks, []byte("after the hello"))
			}
			sendThrough(t, h, tc.authority, auth, chunks...)
			waitFor(t, 2*time.Second, "the destination to receive every byte", func() bool {
				_, got := dest.snapshot()
				return len(got) == 1 && len(got[0]) == len(payload)
			})
			if _, got := dest.snapshot(); !bytes.Equal(got[0], payload) {
				t.Fatalf("the destination got %q, want the peeked bytes unchanged %q", got[0], payload)
			}
			waitFor(t, 2*time.Second, "the ok log line", func() bool {
				return strings.Contains(h.logs.String(), `"outcome":"ok"`)
			})
		})
	}
}

func TestConnectSNIRefusals(t *testing.T) {
	big := helloRecords(sniExt("front.example"), ext(0x0015, make([]byte, 900))) // padding
	cases := []struct {
		name      string
		authority string
		chunks    [][]byte
		reason    string
		alert     alertWant
		opt       harnessOption
	}{
		{name: "mismatch", authority: "front.example:443", chunks: [][]byte{helloRecords(sniExt("hidden.example"))}, reason: ReasonSNIMismatch, alert: alertSent},
		{name: "crypto/tls mismatch", authority: "front.example:443", chunks: [][]byte{captureClientHello(t, &tls.Config{ServerName: "hidden.example", MinVersion: tls.VersionTLS12})}, reason: ReasonSNIMismatch, alert: alertSent},
		{name: "suffix is not a match", authority: "front.example:443", chunks: [][]byte{helloRecords(sniExt("x.front.example"))}, reason: ReasonSNIMismatch, alert: alertSent},
		{name: "name on an IP literal", authority: "203.0.113.51:443", chunks: [][]byte{helloRecords(sniExt("front.example"))}, reason: ReasonSNIMismatch, alert: alertSent},
		{name: "ECH public name differs", authority: "front.example:443", chunks: [][]byte{helloRecords(sniExt("cdn-public.example"), ext(extensionECH, []byte{0, 1, 2}))}, reason: ReasonSNIMismatch, alert: alertSent},
		{name: "mismatch in fragments", authority: "front.example:443", chunks: [][]byte{records(handshake(helloBody(sniExt("hidden.example"))), 2, 2, 2)}, reason: ReasonSNIMismatch, alert: alertSent},
		{name: "no SNI to a name", authority: "front.example:443", chunks: [][]byte{helloRecords()}, reason: ReasonSNIMissing, alert: alertSent},
		{name: "plain bytes on 443", authority: "front.example:443", chunks: [][]byte{[]byte("GET / HTTP/1.1\r\nHost: hidden.example\r\n\r\n")}, reason: ReasonNotTLS, alert: alertNone},
		{name: "malformed", authority: "front.example:443", chunks: [][]byte{helloRecords(sniExt("front.example"), sniExt("front.example"))}, reason: ReasonClientHelloMalformed, alert: alertSent},
		{
			name: "too large", authority: "front.example:443", chunks: [][]byte{big}, reason: ReasonClientHelloTooLarge, alert: alertEither,
			opt: func(o *Options) { o.clientHelloMaxBytes = 512 },
		},
		{
			name: "too large in small records", authority: "front.example:443", reason: ReasonClientHelloTooLarge, alert: alertEither,
			chunks: [][]byte{records(handshake(helloBody(sniExt("front.example"), ext(0x0015, make([]byte, 900)))), 100, 100, 100, 100, 100, 100)},
			opt:    func(o *Options) { o.clientHelloMaxBytes = 512 },
		},
		{
			name: "timeout", authority: "front.example:443", chunks: [][]byte{helloRecords(sniExt("front.example"))[:12]}, reason: ReasonSNITimeout, alert: alertSent,
			opt: func(o *Options) { o.clientHelloTimeout = 200 * time.Millisecond },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []harnessOption
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			h, dest, auth := sniHarness(t, opts...)
			var back []byte
			if tc.reason == ReasonSNITimeout {
				// The client keeps its side open and waits for the proxy.
				resp, c, br := h.rawConnect(tc.authority, auth)
				if resp.StatusCode != 200 {
					t.Fatalf("CONNECT: %d", resp.StatusCode)
				}
				_, _ = c.Write(tc.chunks[0])
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				back, _ = io.ReadAll(br)
				_ = c.Close()
			} else {
				back = sendThrough(t, h, tc.authority, auth, tc.chunks...)
			}
			expectSNIRefusal(t, h, dest, tc.reason, back, tc.alert)
		})
	}
}

// TestConnectSNIRefusalIsInTheRunSummary: a peek refusal is a refused host of
// the run, like any other refusal.
func TestConnectSNIRefusalIsInTheRunSummary(t *testing.T) {
	h, _, auth := sniHarness(t)
	sendThrough(t, h, "front.example:443", auth, helloRecords(sniExt("hidden.example")))
	waitFor(t, 2*time.Second, "the refusal", func() bool { return h.metrics.RefusalCount(ReasonSNIMismatch) == 1 })
	waitFor(t, 2*time.Second, "the tunnel to end", func() bool { return h.metrics.activeTunnels.Load() == 0 })
	h.srv.Runs().Sweep(time.Now(), 0, false)
	var summary string
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, SummaryEvent) {
			summary = line
		}
	}
	if !strings.Contains(summary, `{"host":"front.example","port":443,"reason":"sni-mismatch","count":1}`) {
		t.Fatalf("the run summary lacks the sni-mismatch refusal: %s", summary)
	}
	if strings.Contains(summary, `"hosts_reached":[{`) {
		t.Fatalf("a refused tunnel must not count as reached: %s", summary)
	}
}

// TestConnectRealTLSThroughThePeek: a crypto/tls client verifies the
// destination end to end through a checked tunnel, with a mixed-case name.
func TestConnectRealTLSThroughThePeek(t *testing.T) {
	h := newHarness(t)
	roots := tlsDestination(t, h)
	auth := basic(h.token("exec-real", []string{"example.com"}, time.Hour))
	resp, c, br := h.rawConnect("example.com:443", auth)
	defer func() { _ = c.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %d", resp.StatusCode)
	}
	tc := tls.Client(&bufferedConn{Conn: c, r: br}, &tls.Config{ServerName: "EXAMPLE.com", RootCAs: roots, MinVersion: tls.VersionTLS12})
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake through the checked tunnel: %v", err)
	}
	if _, err := io.WriteString(tc, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(tc)
	if !strings.Contains(string(body), "hello through the tunnel") {
		t.Fatalf("response: %q", body)
	}

	// The same client asking for another name through the tunnel is refused
	// with an alert it understands.
	resp2, c2, br2 := h.rawConnect("example.com:443", auth)
	defer func() { _ = c2.Close() }()
	if resp2.StatusCode != 200 {
		t.Fatalf("CONNECT: %d", resp2.StatusCode)
	}
	tc2 := tls.Client(&bufferedConn{Conn: c2, r: br2}, &tls.Config{ServerName: "other.example", RootCAs: roots, MinVersion: tls.VersionTLS12})
	_ = tc2.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc2.Handshake(); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("a fronted handshake must fail with access denied: %v", err)
	}
}

// bufferedConn reads through the bufio.Reader that read the CONNECT response.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
