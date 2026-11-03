package decoy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// CA is the decoy certificate authority. Its certificate is trusted INSIDE the
// workspace (added to the curated /etc/ssl/certs bundle), so the stub responder
// can present a valid certificate for any intercepted hostname and a TLS client
// sees no warning. A warning is WORSE than nothing: it tells the tenant the
// environment is tampered with.
//
// The CA subject is deliberately neutral and consistent with the decoy profile
// (it names no engine and no platform), so `strings` on the trust store reveals
// nothing about the mechanism.
type CA struct {
	cert    *x509.Certificate
	key     *rsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// NewCA generates a fresh self-signed CA for the decoy layer. The subject is
// derived from the org so it reads as an ordinary hosting company's root.
func NewCA(org string) (*CA, error) {
	if org == "" {
		org = "Example Transit Networks"
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   org + " Root CA",
			Organization: []string{org},
			Country:      []string{"DE"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(5, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return &CA{cert: cert, key: key, CertPEM: certPEM, KeyPEM: keyPEM, leafs: map[string]*tls.Certificate{}}, nil
}

// LoadCA reconstructs a CA from materialized PEM bytes (so a restart reuses the
// same authority and a previously-trusted workspace is not invalidated).
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("decoy CA: no certificate PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("decoy CA: no key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, CertPEM: certPEM, KeyPEM: keyPEM, leafs: map[string]*tls.Certificate{}}, nil
}

// FileName returns a neutral, collision-free filename for the CA certificate, in
// the OpenSSL hashed form so a capath lookup also finds it. It names no engine.
func (ca *CA) FileName() string {
	sum := sha1.Sum(ca.cert.Raw)
	return hex.EncodeToString(sum[:]) + ".0"
}

// parseAddrMaybe returns the net.IP for a string that is an IP literal, else nil.
func parseAddrMaybe(s string) net.IP {
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	return nil
}

// Leaf returns a TLS certificate for a hostname, minted on demand and cached.
// Every intercepted hostname gets a certificate whose SAN is exactly that name,
// so the interception is seamless for curl, wget and python.
func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	return ca.leafFor(host, nil)
}

// LeafForIPs returns a certificate with the given IP addresses as SANs, used
// when a client addresses a bare resolver IP (no SNI): the certificate must
// carry that IP so verification succeeds rather than warning. The set is fixed
// and small (the covered DoH/DoT resolvers), so the extra SANs are harmless.
func (ca *CA) LeafForIPs(ips []string) (*tls.Certificate, error) {
	return ca.leafFor("", ips)
}

func (ca *CA) leafFor(host string, ips []string) (*tls.Certificate, error) {
	cacheKey := host
	if host == "" {
		cacheKey = "ips:" + strings.Join(ips, ",")
	}
	ca.mu.Lock()
	if c, ok := ca.leafs[cacheKey]; ok {
		ca.mu.Unlock()
		return c, nil
	}
	ca.mu.Unlock()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cn := host
	if cn == "" {
		cn = "resolver"
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: ca.cert.Subject.Organization,
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	// A DNS name goes in DNSNames; a bare IP goes in IPAddresses.
	if ip := parseAddrMaybe(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else if host != "" {
		tmpl.DNSNames = []string{host}
	}
	for _, s := range ips {
		if ip := parseAddrMaybe(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}
	ca.mu.Lock()
	ca.leafs[cacheKey] = leaf
	ca.mu.Unlock()
	return leaf, nil
}
