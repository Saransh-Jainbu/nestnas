// Package tlsutil gives NestNAS its own HTTPS (standard library only).
//
// On first run it creates a private "NestNAS Local CA" (ECDSA P-256). The CA is name-constrained:
// it can only ever vouch for private-network addresses (192.168/16, 10/8, 172.16/12, Tailscale's
// 100.64/10, localhost, *.local), so even if its key were stolen it could not be used to
// impersonate a public website to a device that installed it. The server then issues a
// short-lived leaf certificate for every address the laptop currently has, and issues a new
// one automatically when a device connects to an address that isn't covered yet (DHCP change).
// Only TLS 1.3 is accepted.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	dir    string
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte

	mu   sync.Mutex
	leaf *tls.Certificate
	ips  map[string]bool
	exp  time.Time
}

// ifaceAddrs is a variable so tests can simulate a changing network.
var ifaceAddrs = net.InterfaceAddrs

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

// Open loads the CA from dir, creating it on first use.
func Open(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, ips: map[string]bool{}}
	crt, key := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	cb, e1 := os.ReadFile(crt)
	kb, e2 := os.ReadFile(key)
	if e1 == nil && e2 == nil {
		if err := m.parseCA(cb, kb); err == nil {
			return m, nil
		}
	}
	if err := m.createCA(crt, key); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) parseCA(certPEM, keyPEM []byte) error {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return errors.New("bad CA files")
	}
	c, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return err
	}
	k, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return err
	}
	if time.Now().After(c.NotAfter.Add(-30 * 24 * time.Hour)) {
		return errors.New("CA expiring")
	}
	m.caCert, m.caKey, m.caPEM = c, k, certPEM
	return nil
}

func (m *Manager) createCA(crtPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	host = strings.ToLower(host)
	dns := []string{"localhost", "local", "ts.net"}
	if host != "" && !strings.ContainsAny(host, " ._") {
		dns = append(dns, host)
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "NestNAS Local CA", Organization: []string{"NestNAS (" + host + ")"}},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         dns,
		PermittedIPRanges: []*net.IPNet{mustCIDR("10.0.0.0/8"), mustCIDR("172.16.0.0/12"), mustCIDR("192.168.0.0/16"),
			mustCIDR("100.64.0.0/10"), mustCIDR("127.0.0.0/8"), mustCIDR("::1/128"), mustCIDR("fc00::/7"), mustCIDR("fe80::/10")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(crtPath, certPEM, 0o644); err != nil {
		return err
	}
	c, _ := x509.ParseCertificate(der)
	m.caCert, m.caKey, m.caPEM = c, key, certPEM
	return nil
}

// CAPEM is the public CA certificate that devices install to trust this NAS.
func (m *Manager) CAPEM() []byte { return m.caPEM }

// Fingerprint is the CA's SHA-256 fingerprint (for verifying the download on a new device).
func (m *Manager) Fingerprint() string {
	sum := sha256.Sum256(m.caCert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func (m *Manager) ipAllowed(ip net.IP) bool {
	for _, n := range m.caCert.PermittedIPRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (m *Manager) dnsAllowed(name string) bool {
	for _, d := range m.caCert.PermittedDNSDomains {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

func (m *Manager) localIPs() []net.IP {
	var out []net.IP
	addrs, _ := ifaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out
}

// leafFor returns a certificate valid for all current local addresses plus extra.
func (m *Manager) leafFor(extra net.IP) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	want = append(want, m.localIPs()...)
	if extra != nil {
		want = append(want, extra)
	}
	covered := m.leaf != nil && time.Until(m.exp) > 30*24*time.Hour
	for _, ip := range want {
		if covered && m.ipAllowed(ip) && !m.ips[ip.String()] {
			covered = false
		}
	}
	if covered {
		return m.leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	have := map[string]bool{}
	for _, ip := range want {
		if m.ipAllowed(ip) && !have[ip.String()] {
			have[ip.String()] = true
			ips = append(ips, ip)
		}
	}
	host, _ := os.Hostname()
	host = strings.ToLower(host)
	var names []string
	for _, n := range []string{"localhost", host, host + ".local"} {
		if n != "" && n != ".local" && m.dnsAllowed(n) {
			names = append(names, n)
		}
	}
	notAfter := time.Now().AddDate(0, 0, 365)
	if notAfter.After(m.caCert.NotAfter) {
		notAfter = m.caCert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "NestNAS"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: names, IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, err
	}
	m.leaf = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	m.ips, m.exp = have, notAfter
	return m.leaf, nil
}

// TLSConfig returns a server config: TLS 1.3 only, with certificates issued on demand.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			var extra net.IP
			if a, ok := h.Conn.LocalAddr().(*net.TCPAddr); ok {
				extra = a.IP
			}
			return m.leafFor(extra)
		},
	}
}
