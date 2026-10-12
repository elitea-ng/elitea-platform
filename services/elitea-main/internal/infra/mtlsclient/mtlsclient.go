// Package mtlsclient is the ONE mutual-TLS client configuration elitea-main's
// internal gRPC hops share: the platform-operations client of a DeepWiki or
// Inventory engine (providerhost/platformops) and the elitea-vector client
// (infra/vectorclient). Both present a client certificate from files, trust
// a CA bundle from a file and verify a server name; this package owns that
// shape, so a rule it enforces holds on every hop.
//
// THE MATERIAL ROTATES WITHOUT A RESTART. The chart issues these certificates
// with cert-manager, which renews them and rewrites the mounted Secret while
// the pod runs; a process that loaded the files once would present an expired
// leaf, or trust a retired CA, until its next restart. So the material is
// loaded at construction (a missing or malformed file is a construction
// error, not a per-call one) and then RE-READ ON ROTATION: before every
// handshake the three files are stat'ed, and when any one's modification time
// or size differs from the loaded copy's, all three are reloaded and the
// handshake uses the new leaf and the new roots. A reload that fails (a Secret
// half-rewritten, a key that does not match its new certificate) keeps the
// last good material for this handshake and is retried at the next, because
// the stamps are only advanced by a reload that parsed.
//
// This is done by building the tls.Config PER HANDSHAKE from the current
// material, not with GetClientCertificate alone: the roots a tls.Config
// verifies against (RootCAs) are fixed at handshake time, so a rotated CA
// needs a fresh configuration, and one path that rebuilds both is simpler
// than two that rebuild one each. The configuration is otherwise what the
// hops always used: TLS 1.2 at least, the server name verified, no
// InsecureSkipVerify anywhere.
package mtlsclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"google.golang.org/grpc/credentials"
)

// Material names the three files and the server name of one mutual-TLS hop.
type Material struct {
	// CertFile and KeyFile are the client certificate this process presents
	// and its key; CAFile is the bundle that signed the server's certificate.
	CertFile string
	KeyFile  string
	CAFile   string
	// ServerName is verified against the server's certificate. Empty is the
	// host of Target.
	ServerName string
	// Target is the dialled host:port; its host is the server name when
	// ServerName is empty.
	Target string
}

// Validate reports the first thing that makes the material unusable before
// any file is read.
func (m Material) Validate() error {
	for _, file := range []struct{ name, value string }{
		{"client certificate", m.CertFile},
		{"client key", m.KeyFile},
		{"CA bundle", m.CAFile},
	} {
		if strings.TrimSpace(file.value) == "" {
			return fmt.Errorf("the mTLS client needs a %s file", file.name)
		}
	}
	if m.serverName() == "" {
		return errors.New("the mTLS client needs a server name or a host:port target")
	}
	return nil
}

func (m Material) serverName() string {
	if m.ServerName != "" {
		return m.ServerName
	}
	host, _, err := net.SplitHostPort(m.Target)
	if err != nil {
		return ""
	}
	return host
}

// stamp is what a file is compared by between handshakes.
type stamp struct {
	size    int64
	modTime int64
}

func stampOf(path string) (stamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return stamp{}, err
	}
	return stamp{size: info.Size(), modTime: info.ModTime().UnixNano()}, nil
}

// loaded is one parsed set of material and the stamps it was read at.
type loaded struct {
	stamps [3]stamp
	cert   tls.Certificate
	roots  *x509.CertPool
}

// Reloader holds the material of one hop and hands out the current TLS
// configuration, re-reading the files when they change.
type Reloader struct {
	material Material
	mu       sync.Mutex
	current  loaded
}

// NewReloader loads the material now and fails on anything it cannot use.
func NewReloader(material Material) (*Reloader, error) {
	if err := material.Validate(); err != nil {
		return nil, err
	}
	r := &Reloader{material: material}
	stamps, err := r.stamps()
	if err != nil {
		return nil, err
	}
	current, err := r.load(stamps)
	if err != nil {
		return nil, err
	}
	r.current = current
	return r, nil
}

func (r *Reloader) stamps() ([3]stamp, error) {
	var stamps [3]stamp
	for i, path := range []string{r.material.CertFile, r.material.KeyFile, r.material.CAFile} {
		s, err := stampOf(path)
		if err != nil {
			return stamps, fmt.Errorf("stat mTLS material: %w", err)
		}
		stamps[i] = s
	}
	return stamps, nil
}

func (r *Reloader) load(stamps [3]stamp) (loaded, error) {
	cert, err := tls.LoadX509KeyPair(r.material.CertFile, r.material.KeyFile)
	if err != nil {
		return loaded{}, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(r.material.CAFile)
	if err != nil {
		return loaded{}, fmt.Errorf("read ca bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return loaded{}, fmt.Errorf("no certificates parsed from ca bundle %q", r.material.CAFile)
	}
	return loaded{stamps: stamps, cert: cert, roots: roots}, nil
}

// Config returns the TLS configuration for one handshake, built from the
// material as it is on disk now. A changed file is reloaded first; a reload
// that fails leaves the last good material in place and is reported as the
// second result, so a caller can log it while the handshake proceeds.
func (r *Reloader) Config() (*tls.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var reloadErr error
	if stamps, err := r.stamps(); err != nil {
		reloadErr = err
	} else if stamps != r.current.stamps {
		if fresh, err := r.load(stamps); err != nil {
			reloadErr = fmt.Errorf("mTLS material changed on disk but could not be reloaded; the last good material is used: %w", err)
		} else {
			r.current = fresh
		}
	}
	return &tls.Config{
		Certificates: []tls.Certificate{r.current.cert},
		RootCAs:      r.current.roots,
		ServerName:   r.material.serverName(),
		MinVersion:   tls.VersionTLS12,
	}, reloadErr
}

// Credentials are gRPC transport credentials over a Reloader: every
// connection's handshake uses the material as it is on disk at that moment.
type Credentials struct {
	reloader   *Reloader
	serverName string
	// onReloadError, when set, is told about a reload that failed (the
	// handshake still proceeds with the last good material).
	onReloadError func(error)
}

// NewCredentials loads the material and returns client transport credentials
// for grpc.WithTransportCredentials. onReloadError may be nil.
func NewCredentials(material Material, onReloadError func(error)) (*Credentials, error) {
	reloader, err := NewReloader(material)
	if err != nil {
		return nil, err
	}
	return &Credentials{reloader: reloader, serverName: material.serverName(), onReloadError: onReloadError}, nil
}

// ClientHandshake implements credentials.TransportCredentials with a
// configuration built from the current material.
func (c *Credentials) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg, reloadErr := c.reloader.Config()
	if reloadErr != nil && c.onReloadError != nil {
		c.onReloadError(reloadErr)
	}
	cfg.ServerName = c.serverName
	return credentials.NewTLS(cfg).ClientHandshake(ctx, authority, rawConn)
}

// ServerHandshake is not supported: these are client credentials.
func (c *Credentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("mtlsclient: client credentials cannot accept connections")
}

// Info implements credentials.TransportCredentials.
func (c *Credentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.2", ServerName: c.serverName}
}

// Clone implements credentials.TransportCredentials; the clone shares the
// reloader, so one set of files is stat'ed and loaded once per change.
func (c *Credentials) Clone() credentials.TransportCredentials {
	clone := *c
	return &clone
}

// OverrideServerName implements credentials.TransportCredentials.
func (c *Credentials) OverrideServerName(name string) error {
	c.serverName = name
	return nil
}

var _ credentials.TransportCredentials = (*Credentials)(nil)
