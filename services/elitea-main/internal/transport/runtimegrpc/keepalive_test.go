package runtimegrpc

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// The Worker's probe schedule. These mirror HTTP2_KEEPALIVE_INTERVAL and
// HTTP2_KEEPALIVE_TIMEOUT in services/elitea-worker-rust/src/transport/keepalive.rs,
// and TestWorkerKeepaliveConstantsMatchTheRustSource fails if either side moves.
const (
	workerHTTP2KeepaliveInterval = 15 * time.Second
	workerHTTP2KeepaliveTimeout  = 5 * time.Second
	// The shortest claim lease a dead peer must be found within.
	minClaimLease = time.Minute
)

func TestRuntimeKeepalivePolicyValuesAndInvariant(t *testing.T) {
	policy := RuntimeKeepaliveEnforcementPolicy()
	if policy.MinTime != 10*time.Second || !policy.PermitWithoutStream {
		t.Fatalf("production policy = %+v, want MinTime 10s and PermitWithoutStream true", policy)
	}
	if policy.MinTime != RuntimeKeepaliveMinTime || policy.PermitWithoutStream != RuntimeKeepalivePermitWithoutStream {
		t.Fatal("the constructor does not use the named constants")
	}
	// MinTime must stay below the Worker interval with a margin, or the Worker's
	// idle ping is a strike.
	if policy.MinTime >= workerHTTP2KeepaliveInterval || workerHTTP2KeepaliveInterval-policy.MinTime < 2*time.Second {
		t.Fatalf("MinTime %v leaves no margin below the Worker ping interval %v", policy.MinTime, workerHTTP2KeepaliveInterval)
	}
	if detection := workerHTTP2KeepaliveInterval + workerHTTP2KeepaliveTimeout; detection != 20*time.Second || detection >= minClaimLease {
		t.Fatalf("dead-peer detection %v is not within the claim lease %v", detection, minClaimLease)
	}
	scaled := newRuntimeKeepalivePolicy(123 * time.Millisecond)
	if scaled.MinTime != 123*time.Millisecond || scaled.PermitWithoutStream != RuntimeKeepalivePermitWithoutStream {
		t.Fatalf("scaled policy = %+v", scaled)
	}
}

// The Go test above pins Main's side; this pins the Rust side. Changing the
// Worker interval or timeout without updating this package fails here.
func TestWorkerKeepaliveConstantsMatchTheRustSource(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "elitea-worker-rust", "src", "transport", "keepalive.rs")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("Worker source not present in this checkout (%s); the Rust side pins its own copy of the Main minimum", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for name, want := range map[string]time.Duration{
		"HTTP2_KEEPALIVE_INTERVAL": workerHTTP2KeepaliveInterval,
		"HTTP2_KEEPALIVE_TIMEOUT":  workerHTTP2KeepaliveTimeout,
	} {
		match := regexp.MustCompile(`const ` + name + `: Duration = Duration::from_secs\((\d+)\);`).FindStringSubmatch(text)
		if match == nil || match[1] != formatSeconds(want) {
			t.Errorf("%s in keepalive.rs = %v, want %s s", name, match, formatSeconds(want))
		}
	}
	if !regexp.MustCompile(`const KEEPALIVE_WHILE_IDLE: bool = true;`).MatchString(text) {
		t.Error("the Worker no longer pings while idle; RuntimeKeepalivePermitWithoutStream may be relaxed deliberately")
	}
	match := regexp.MustCompile(`const MAIN_ENFORCEMENT_MIN_TIME: Duration = Duration::from_secs\((\d+)\);`).FindStringSubmatch(text)
	if match == nil || match[1] != formatSeconds(RuntimeKeepaliveMinTime) {
		t.Errorf("the Worker's copy of Main's minimum = %v, want %s s", match, formatSeconds(RuntimeKeepaliveMinTime))
	}
}

func formatSeconds(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }

// Every private runtime server takes its enforcement policy from the one
// constructor. A literal EnforcementPolicy anywhere else in Main would let the
// two sides drift again.
func TestEveryMainGRPCServerUsesTheSingleEnforcementConstructor(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "keepalive.go" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte("keepalive.EnforcementPolicy{")) || bytes.Contains(body, []byte("MinClientPingInterval")) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("enforcement policy defined outside keepalive.go: %v", offenders)
	}
	body, err := os.ReadFile("private_servers.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("grpc.KeepaliveEnforcementPolicy(RuntimeKeepaliveEnforcementPolicy())")) {
		t.Fatal("private runtime servers do not use RuntimeKeepaliveEnforcementPolicy")
	}
}

// h2Probe is a bare HTTP/2 client: the Worker's idle-channel PING, with the
// interval scaled down. It opens no stream, like a channel with no call in flight.
type h2Probe struct {
	conn net.Conn
	// framer writes only, under writeMu: the reader goroutine acknowledges
	// SETTINGS while the test pings, and a Framer is not safe for concurrent writes.
	framer  *http2.Framer
	writeMu sync.Mutex
	acks    chan struct{}
	goaway  chan http2.GoAwayFrame
	closed  chan error
	goDebug chan string
}

func dialH2Probe(t *testing.T, address string) *h2Probe {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err = io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(conn, nil)
	if err = framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	reader := http2.NewFramer(nil, conn)
	probe := &h2Probe{conn: conn, framer: framer, acks: make(chan struct{}, 64), goaway: make(chan http2.GoAwayFrame, 1), closed: make(chan error, 1), goDebug: make(chan string, 1)}
	go func() {
		for {
			frame, err := reader.ReadFrame()
			if err != nil {
				probe.closed <- err
				return
			}
			switch f := frame.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					probe.writeMu.Lock()
					_ = framer.WriteSettingsAck()
					probe.writeMu.Unlock()
				}
			case *http2.PingFrame:
				if f.IsAck() {
					probe.acks <- struct{}{}
				}
			case *http2.GoAwayFrame:
				probe.goDebug <- string(f.DebugData())
				probe.goaway <- *f
			}
		}
	}()
	return probe
}

func (p *h2Probe) writePing() error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.framer.WritePing(false, [8]byte{1, 2, 3, 4, 5, 6, 7, 8})
}

func (p *h2Probe) ping(t *testing.T) {
	t.Helper()
	if err := p.writePing(); err != nil {
		t.Fatalf("ping write failed, the server already closed the connection: %v", err)
	}
}

func startBareGRPCServer(t *testing.T, policy keepalive.EnforcementPolicy) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(policy))
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); <-done })
	return listener.Addr().String()
}

const (
	probeInterval = 150 * time.Millisecond
	// More than three strike windows of the scaled MinTime (100 ms) and many
	// more than the two strikes grpc-go tolerates.
	probePings = 10
)

// The production constructor, with time scaled 100x down (MinTime 100 ms, Worker
// ping 150 ms), on a real grpc-go server. An idle, stream-less client that pings
// slower than MinTime keeps its connection past ten pings and gets every ack.
func TestRuntimeKeepalivePolicyAcceptsIdleWorkerPingsOnARealServer(t *testing.T) {
	address := startBareGRPCServer(t, newRuntimeKeepalivePolicy(100*time.Millisecond))
	probe := dialH2Probe(t, address)
	for i := 0; i < probePings; i++ {
		probe.ping(t)
		select {
		case <-probe.acks:
		case frame := <-probe.goaway:
			t.Fatalf("GOAWAY after %d idle pings: code=%v debug=%q", i, frame.ErrCode, <-probe.goDebug)
		case err := <-probe.closed:
			t.Fatalf("connection closed after %d idle pings: %v", i, err)
		case <-time.After(2 * time.Second):
			t.Fatalf("ping %d was not acknowledged", i)
		}
		time.Sleep(probeInterval)
	}
	select {
	case frame := <-probe.goaway:
		t.Fatalf("GOAWAY on a compliant idle client: %v", frame.ErrCode)
	case err := <-probe.closed:
		t.Fatalf("connection closed on a compliant idle client: %v", err)
	default:
	}
}

// Negative controls: the same probe against the old policy shape, and against a
// MinTime above the ping interval, is closed with GOAWAY too_many_pings. Without
// them the test above could pass against a server that never enforces anything.
func TestOldRuntimeKeepalivePolicyClosesIdleWorkerPings(t *testing.T) {
	for name, policy := range map[string]keepalive.EnforcementPolicy{
		// The previous production policy: a 30 s floor (scaled to 1 s, above the
		// ping interval) and no pings without a stream.
		"old policy: floor above ping interval, no idle pings": {MinTime: time.Second, PermitWithoutStream: false},
		// Idle pings allowed, but the floor is above the Worker interval.
		"floor above the ping interval": {MinTime: time.Second, PermitWithoutStream: true},
		// A floor below the interval but idle pings refused.
		"idle pings refused": {MinTime: 100 * time.Millisecond, PermitWithoutStream: false},
	} {
		t.Run(name, func(t *testing.T) {
			address := startBareGRPCServer(t, policy)
			probe := dialH2Probe(t, address)
			deadline := time.After(5 * time.Second)
			for i := 0; ; i++ {
				select {
				case frame := <-probe.goaway:
					debug := <-probe.goDebug
					if frame.ErrCode != http2.ErrCodeEnhanceYourCalm || debug != "too_many_pings" {
						t.Fatalf("GOAWAY code=%v debug=%q, want ENHANCE_YOUR_CALM too_many_pings", frame.ErrCode, debug)
					}
					if i > probePings {
						t.Fatalf("GOAWAY only after %d pings", i)
					}
					return
				case <-deadline:
					t.Fatal("an idle client pinging faster than the policy allows was never refused")
				default:
				}
				if err := probe.writePing(); err != nil {
					// The server closed the socket right after its GOAWAY; the reader
					// goroutine may not have delivered the frame yet.
					select {
					case frame := <-probe.goaway:
						if frame.ErrCode != http2.ErrCodeEnhanceYourCalm || <-probe.goDebug != "too_many_pings" {
							t.Fatalf("GOAWAY code=%v", frame.ErrCode)
						}
						return
					case <-time.After(2 * time.Second):
						t.Fatalf("write failed without GOAWAY: %v", err)
					}
				}
				time.Sleep(probeInterval)
			}
		})
	}
}
