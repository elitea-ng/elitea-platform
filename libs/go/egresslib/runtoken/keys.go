package runtoken

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxKeyFileBytes bounds one key file. A PEM key is a few hundred bytes.
const maxKeyFileBytes = 64 << 10

// ParsePrivateKeyPEM reads one PKCS#8 "PRIVATE KEY" PEM block holding an
// Ed25519 key, as `openssl genpkey -algorithm ed25519` writes it.
func ParsePrivateKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("runtoken: want a PEM \"PRIVATE KEY\" block (PKCS#8)")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("runtoken: the PRIVATE KEY block is not valid PKCS#8")
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("runtoken: the private key is not Ed25519")
	}
	return ed, nil
}

// LoadPrivateKeyFile reads ParsePrivateKeyPEM's format from a file, such as a
// mounted Secret.
func LoadPrivateKeyFile(path string) (ed25519.PrivateKey, error) {
	data, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	return ParsePrivateKeyPEM(data)
}

// MarshalPrivateKeyPEM writes a private key in ParsePrivateKeyPEM's format.
func MarshalPrivateKeyPEM(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// MarshalPublicKeyPEM writes a public key as a PKIX "PUBLIC KEY" PEM block,
// as `openssl pkey -pubout` writes it.
func MarshalPublicKeyPEM(key ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// ParsePublicKeysPEM reads every PKIX "PUBLIC KEY" block in data. Each must
// hold an Ed25519 key. Text outside the blocks is ignored, so one file may
// carry comments between keys. No block is an error.
func ParsePublicKeysPEM(data []byte) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("runtoken: want PEM \"PUBLIC KEY\" blocks, found %q", block.Type)
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("runtoken: a PUBLIC KEY block is not valid PKIX")
		}
		ed, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("runtoken: a public key is not Ed25519")
		}
		keys = append(keys, ed)
	}
	if len(keys) == 0 {
		return nil, errors.New("runtoken: no PUBLIC KEY block found")
	}
	return keys, nil
}

// LoadKeySet reads public keys from a file, or from every regular file in a
// directory (a mounted Secret with one key per data item). In a directory,
// names that start with '.' are skipped: Kubernetes keeps its atomic-update
// links there (..data, ..2026_10_15...).
func LoadKeySet(path string) (*KeySet, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("runtoken: read the key set: %w", err)
	}
	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("runtoken: read the key directory: %w", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(path, e.Name())
			st, err := os.Stat(full) // follows the Secret's symlinks
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			files = append(files, full)
		}
		sort.Strings(files)
	} else {
		files = []string{path}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("runtoken: no key file in %s", path)
	}
	var keys []ed25519.PublicKey
	for _, f := range files {
		data, err := readBounded(f)
		if err != nil {
			return nil, err
		}
		got, err := ParsePublicKeysPEM(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		keys = append(keys, got...)
	}
	return NewKeySet(keys...)
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("runtoken: open key file: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("runtoken: read key file: %w", err)
	}
	if len(data) > maxKeyFileBytes {
		return nil, fmt.Errorf("runtoken: key file %s is larger than %d bytes", filepath.Base(path), maxKeyFileBytes)
	}
	return data, nil
}
