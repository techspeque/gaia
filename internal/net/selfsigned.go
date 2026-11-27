package net

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

// Self-signed certificate files in the tsnet state directory.
const (
	certFile = "gaia-tls.crt"
	keyFile  = "gaia-tls.key"
)

// selfSignedValidity is how long a generated certificate is valid. Clients
// pin it rather than chain it to a CA, so a long life costs nothing.
const selfSignedValidity = 10 * 365 * 24 * time.Hour

// CertPath returns the self-signed certificate's path for the state
// directory dir (Config.Dir), so a client can pin it.
func CertPath(dir string) string { return filepath.Join(dir, stateSubdir, certFile) }

// loadOrCreateSelfSigned loads the certificate kept in dir, or generates an
// ECDSA P-256 certificate for names (DNS names or IP addresses) and writes
// it there, both files 0600, when none exists or the kept one has expired.
// A kept pair that does not parse is an error, not overwritten.
func loadOrCreateSelfSigned(dir string, names []string) (*tls.Certificate, error) {
	crt, key := filepath.Join(dir, certFile), filepath.Join(dir, keyFile)
	c, err := tls.LoadX509KeyPair(crt, key)
	switch {
	case err == nil:
		if time.Now().Before(c.Leaf.NotAfter) {
			return &c, nil
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "gaia"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, n := range names {
		if ip, err := netip.ParseAddr(n); err == nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip.AsSlice())
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeFile0600(key, keyPEM); err != nil {
		return nil, err
	}
	if err := writeFile0600(crt, crtPEM); err != nil {
		return nil, err
	}
	out, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// writeFile0600 replaces path with b through a temporary file, mode 0600.
func writeFile0600(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
