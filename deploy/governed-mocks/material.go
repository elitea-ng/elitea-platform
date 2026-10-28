package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mintTLS writes a two-level chain: a CA (ca.crt) and a leaf for every host
// (server-chain.crt, server.key). It runs at image build time, so the bytes
// survive restarts and the trust bundle handed to the worker and Main never
// goes stale while a run is in flight (the deploy/mock-mcp rule). The CA key
// is not written: nothing can mint a second certificate from the image.
func mintTLS(args []string) error {
	flags := flag.NewFlagSet("mint-tls", flag.ContinueOnError)
	out := flags.String("out", "", "output directory")
	hosts := flags.String("hosts", "", "comma-separated DNS names and IPs for the leaf")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" || *hosts == "" {
		return errors.New("mint-tls needs -out and -hosts")
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now().Add(-time.Hour)
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "elitea-governed-mocks-ca"},
		NotBefore:             now,
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "governed-mocks"},
		NotBefore:    now,
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range strings.Split(*hosts, ",") {
		host = strings.TrimSpace(host)
		if ip := net.ParseIP(host); ip != nil {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		} else if host != "" {
			leaf.DNSNames = append(leaf.DNSNames, host)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	chain := append(pemBlock("CERTIFICATE", leafDER), pemBlock("CERTIFICATE", caDER)...)
	// 0644 on the key, as deploy/mock-mcp does: the server runs non-root, the
	// material is a throwaway for one test double on a private network.
	for name, body := range map[string][]byte{
		"ca.crt":           pemBlock("CERTIFICATE", caDER),
		"server-chain.crt": chain,
		"server.key":       pemBlock("PRIVATE KEY", leafKeyDER),
	} {
		if err := os.WriteFile(filepath.Join(*out, name), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// mintMaterial writes the stack's run-time test material into -out:
//
//	ca.crt                the mocks' trust anchor (copied from -ca)
//	vault-jwt-public.pem  the key OpenBao's JWT auth method trusts
//	vault-token.jwt       the workload token Main logs in to OpenBao with
//	agent-es256.pem       the Elitea-native agent assertion signing key
//	openbao-tls/          OpenBao's listener chain (ca.crt is Main's CA file)
//
// Every file is 0444 and the directory 0755: Main refuses key material that
// group or other can write. Idempotent: an existing file is kept, so a
// restarted stack keeps the key OpenBao was configured with.
func mintMaterial(args []string) error {
	flags := flag.NewFlagSet("mint-material", flag.ContinueOnError)
	out := flags.String("out", "", "output directory")
	caFile := flags.String("ca", "/opt/governed-mocks/tls/ca.crt", "the mocks' CA certificate")
	issuer := flags.String("vault-issuer", "elitea-governed-conformance", "iss of the workload token")
	subject := flags.String("vault-subject", "system:serviceaccount:elitea:elitea-main", "sub of the workload token")
	audience := flags.String("vault-audience", "elitea-secretstore", "aud of the workload token")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("mint-material needs -out")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(*out, 0o755); err != nil {
		return err
	}
	ca, err := os.ReadFile(*caFile)
	if err != nil {
		return err
	}
	if err := writeOnce(filepath.Join(*out, "ca.crt"), func() ([]byte, error) { return ca, nil }); err != nil {
		return err
	}
	vaultKeyPath := filepath.Join(*out, "vault-jwt-signing.pem")
	if err := writeOnce(vaultKeyPath, func() ([]byte, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		return pemBlock("PRIVATE KEY", der), err
	}); err != nil {
		return err
	}
	raw, err := os.ReadFile(vaultKeyPath)
	if err != nil {
		return err
	}
	vaultKey, err := parseECKey(raw)
	if err != nil {
		return err
	}
	if err := writeOnce(filepath.Join(*out, "vault-jwt-public.pem"), func() ([]byte, error) {
		der, err := x509.MarshalPKIXPublicKey(&vaultKey.PublicKey)
		return pemBlock("PUBLIC KEY", der), err
	}); err != nil {
		return err
	}
	// The workload token is re-minted on every run of the one-shot (it
	// expires); the key it is signed with is not.
	now := time.Now()
	token, err := signES256(vaultKey, "", map[string]any{
		"iss": *issuer, "sub": *subject, "aud": *audience,
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(30 * 24 * time.Hour).Unix(),
	})
	if err != nil {
		return err
	}
	tokenPath := filepath.Join(*out, "vault-token.jwt")
	_ = os.Remove(tokenPath)
	if err := os.WriteFile(tokenPath, []byte(token), 0o444); err != nil {
		return err
	}
	// OpenBao's listener chain. Dev mode's own -dev-tls names localhost
	// only, and Main dials it as `openbao`.
	if _, err := os.Stat(filepath.Join(*out, "openbao-tls", "ca.crt")); err != nil {
		if err := mintTLS([]string{"-out", filepath.Join(*out, "openbao-tls"), "-hosts", "openbao,localhost,127.0.0.1"}); err != nil {
			return err
		}
	}
	return writeOnce(filepath.Join(*out, "agent-es256.pem"), func() ([]byte, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		return pemBlock("PRIVATE KEY", der), err
	})
}

func writeOnce(path string, body func() ([]byte, error)) error {
	if _, err := os.Stat(path); err == nil {
		return os.Chmod(path, 0o444)
	}
	content, err := body()
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o444)
}

func pemBlock(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

func parseECKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA key")
	}
	return key, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		panic(err)
	}
	return n
}

// signES256 is a compact JWS over claims with an ES256 key.
func signES256(key *ecdsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header := map[string]any{"alg": "ES256", "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	signingInput, err := jwsInput(header, claims)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + b64(signature), nil
}

func jwsInput(header, claims map[string]any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return b64(h) + "." + b64(c), nil
}

// ecJWK is the public JWK of an ES256 key.
func ecJWK(key *ecdsa.PublicKey, kid string) map[string]string {
	x := make([]byte, 32)
	y := make([]byte, 32)
	key.X.FillBytes(x)
	key.Y.FillBytes(y)
	return map[string]string{"kty": "EC", "crv": "P-256", "x": b64(x), "y": b64(y), "kid": kid, "alg": "ES256", "use": "sig"}
}
