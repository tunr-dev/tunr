package connector

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"

	"github.com/tunr-dev/tunr/internal/auth"
)

// e2e.go — end-to-end TLS between `tunr access` and this connector (P8-T1).
//
// The connector holds a long-lived TLS key and self-signed certificate (in the
// secret store) and announces the certificate's SHA-256 in its hello. When an
// agent asks for E2E, the relay passes that fingerprint to the agent, which
// does a TLS 1.3 handshake with the connector *through* the stream and pins
// the certificate. The relay then carries ciphertext only.
//
// What this protects: anything that can read the relay passively — logs,
// memory, a curious operator, a recorded stream. What it does not: a relay
// that is actively compromised could hand the agent a different fingerprint
// and sit in the middle. Closing that needs the fingerprint to reach the
// vendor out of band (it is shown in the portal and by `tunr connect status`).

// SiteTLSSecret is the secret-store name of the connector's key + certificate.
const SiteTLSSecret = "site-tls"

// LoadOrCreateSiteTLS returns the connector's certificate, generating and
// storing one (ECDSA P-256, 10 years) on first use.
func LoadOrCreateSiteTLS(store auth.SecretStore, site string) (tls.Certificate, string, error) {
	if pemData, err := store.Get(SiteTLSSecret); err == nil {
		cert, err := tls.X509KeyPair([]byte(pemData), []byte(pemData))
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("stored site TLS key is unreadable: %w", err)
		}
		return cert, Fingerprint(cert.Certificate[0]), nil
	} else if !errors.Is(err, auth.ErrSecretNotFound) {
		return tls.Certificate{}, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tunr site " + site},
		DNSNames:     []string{"tunr-site"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	pemData := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err := store.Set(SiteTLSSecret, pemData); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair([]byte(pemData), []byte(pemData))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, Fingerprint(der), nil
}

// Fingerprint is the hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// wrapTLS terminates the agent's TLS on the connector: it returns the
// relay-facing end of a pipe whose other end is a TLS server that relays
// plaintext to target. Closing either side tears the chain down.
func wrapTLS(target net.Conn, cert tls.Certificate) net.Conn {
	relaySide, tlsSide := net.Pipe()
	go func() {
		defer target.Close()
		srv := tls.Server(tlsSide, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		})
		defer srv.Close()
		_ = srv.SetDeadline(time.Now().Add(15 * time.Second))
		if err := srv.Handshake(); err != nil {
			return
		}
		_ = srv.SetDeadline(time.Time{})
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(target, srv); done <- struct{}{} }()
		go func() { _, _ = io.Copy(srv, target); done <- struct{}{} }()
		<-done
	}()
	return relaySide
}

// WrapTLSForTest exposes wrapTLS to other packages' tests.
func WrapTLSForTest(target net.Conn, cert tls.Certificate) net.Conn { return wrapTLS(target, cert) }
