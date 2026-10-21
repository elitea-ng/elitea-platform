package runtimegrpc

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

const maxRuntimeTLSFileBytes = 1 << 20

type ServerTLSFiles struct {
	CertificateChainPath string
	PrivateKeyPath       string
	ClientCAPath         string
}

// LoadServerTLSConfig loads one fail-closed private-listener mTLS profile.
// Call it independently for control, output and content listeners so their
// certificates and trust roots can be rotated without sharing mutable state.
func LoadServerTLSConfig(files ServerTLSFiles) (*tls.Config, error) {
	if files.CertificateChainPath == "" || files.PrivateKeyPath == "" || files.ClientCAPath == "" {
		return nil, errors.New("server certificate, private key and client CA files are required")
	}
	certificatePEM, err := securefile.Read(files.CertificateChainPath, maxRuntimeTLSFileBytes, securefile.PublicMaterial)
	if err != nil {
		return nil, fmt.Errorf("load runtime server certificate: %w", err)
	}
	privateKeyPEM, err := securefile.Read(files.PrivateKeyPath, maxRuntimeTLSFileBytes, securefile.PrivateMaterial)
	if err != nil {
		return nil, fmt.Errorf("load runtime server private key: %w", err)
	}
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse runtime server key pair: %w", err)
	}
	clientCAPEM, err := securefile.Read(files.ClientCAPath, maxRuntimeTLSFileBytes, securefile.PublicMaterial)
	if err != nil {
		return nil, fmt.Errorf("load runtime client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(clientCAPEM) {
		return nil, errors.New("runtime client CA file contains no certificates")
	}
	return NewServerTLSConfig(certificate, clientCAs)
}

// LoadPlatformServerTLSConfig loads the platform listener's profile. The
// platform listener is a gRPC server (#1359 D1), so this is exactly the
// private gRPC listeners' fail-closed mutual TLS: TLS 1.3, ALPN h2 only, a
// client certificate that chains to the runtime CA required at the
// handshake. It is a separate name so the platform material can rotate on its
// own.
func LoadPlatformServerTLSConfig(files ServerTLSFiles) (*tls.Config, error) {
	return LoadServerTLSConfig(files)
}

func NewServerTLSConfig(certificate tls.Certificate, clientCAs *x509.CertPool) (*tls.Config, error) {
	if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil || clientCAs == nil {
		return nil, errors.New("runtime server key pair and client CA pool are required")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs.Clone(),
		NextProtos:   []string{"h2"},
	}, nil
}

func validateServerTLSConfig(config *tls.Config) error {
	if config == nil || config.MinVersion < tls.VersionTLS13 || config.ClientAuth != tls.RequireAndVerifyClientCert || config.ClientCAs == nil {
		return errors.New("runtime listener requires TLS 1.3 mutual authentication")
	}
	// A per-client replacement config could silently weaken ClientAuth, roots,
	// protocol or version after this validation. Certificate rotation may use
	// GetCertificate, but the security policy itself remains immutable.
	if config.GetConfigForClient != nil {
		return errors.New("runtime listener cannot replace its TLS policy per client")
	}
	if len(config.Certificates) == 0 && config.GetCertificate == nil {
		return errors.New("runtime listener server certificate is required")
	}
	if len(config.NextProtos) != 1 || config.NextProtos[0] != "h2" {
		return errors.New("runtime listener requires h2 ALPN")
	}
	return nil
}
