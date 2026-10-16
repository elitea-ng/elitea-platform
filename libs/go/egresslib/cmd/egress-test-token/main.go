// Command egress-test-token makes a throwaway run-token key pair and one
// token, for the egress proxy's live proofs in CI (ci-rust.yml,
// workspace-run-docker). It is a test tool: production tokens are minted by
// elitea-main (internal/application/egresstoken) with an operator's key.
//
//	egress-test-token -keys-dir DIR -execution ID -project N -allow HOST[,HOST] [-ttl 30m]
//
// It writes DIR/test.pem (the PUBLIC key, for ELITEA_EGRESS_PROXY_PUBLIC_KEYS)
// and prints the token on stdout. The private key is never written.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/runtoken"
)

func main() {
	keysDir := flag.String("keys-dir", "", "directory for the public key")
	execution := flag.String("execution", "ci-execution", "execution id")
	project := flag.Int64("project", 1, "project id")
	allow := flag.String("allow", "", "comma-separated allowlist")
	ttl := flag.Duration("ttl", 30*time.Minute, "token lifetime")
	flag.Parse()
	if *keysDir == "" || *allow == "" {
		fmt.Fprintln(os.Stderr, "usage: egress-test-token -keys-dir DIR -allow HOST[,HOST]")
		os.Exit(2)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail(err)
	}
	pemBytes, err := runtoken.MarshalPublicKeyPEM(pub)
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(*keysDir, 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(*keysDir, "test.pem"), pemBytes, 0o644); err != nil { //nolint:gosec // a public key
		fail(err)
	}
	signer, err := runtoken.NewSigner(priv)
	if err != nil {
		fail(err)
	}
	nonce, err := runtoken.NewNonce()
	if err != nil {
		fail(err)
	}
	now := time.Now().Truncate(time.Second)
	token, err := signer.Sign(runtoken.Claims{
		ExecutionID: *execution, ProjectID: *project, Allowlist: strings.Split(*allow, ","),
		IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(*ttl), Nonce: nonce,
	})
	if err != nil {
		fail(err)
	}
	fmt.Print(token)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "egress-test-token:", err)
	os.Exit(1)
}
