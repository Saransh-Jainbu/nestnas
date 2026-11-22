package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeNet(ips ...string) {
	ifaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, s := range ips {
			out = append(out, &net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(24, 32)})
		}
		return out, nil
	}
}

func pool(m *Manager) *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(m.CAPEM())
	return p
}

func parse(t *testing.T, c *tls.Certificate) *x509.Certificate {
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestLeafValidForPrivateAddresses(t *testing.T) {
	defer func() { ifaceAddrs = net.InterfaceAddrs }()
	fakeNet("192.168.1.20", "100.101.102.103", "8.8.8.8")
	m, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.leafFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf := parse(t, c)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool(m)}); err != nil {
		t.Fatalf("leaf does not chain to the CA: %v", err)
	}
	for _, h := range []string{"192.168.1.20", "100.101.102.103", "127.0.0.1", "localhost"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("not valid for %s: %v", h, err)
		}
	}
	if leaf.VerifyHostname("8.8.8.8") == nil {
		t.Error("a PUBLIC address must never be in the certificate")
	}
}

func TestCANameConstraintsBlockPublicNames(t *testing.T) {
	m, _ := Open(t.TempDir())
	// Even with the CA's own key, a cert for a public site must fail verification.
	m.ips = map[string]bool{}
	for _, bad := range []string{"google.com", "bank.example.org"} {
		if m.dnsAllowed(bad) {
			t.Errorf("%s should not be permitted", bad)
		}
	}
	if m.ipAllowed(net.ParseIP("1.1.1.1")) || !m.ipAllowed(net.ParseIP("10.1.2.3")) {
		t.Error("IP constraints wrong")
	}
	if !m.caCert.PermittedDNSDomainsCritical || !m.caCert.MaxPathLenZero {
		t.Error("CA must be constrained and unable to issue sub-CAs")
	}
}

func TestPersistsAndRegeneratesOnNewIP(t *testing.T) {
	defer func() { ifaceAddrs = net.InterfaceAddrs }()
	dir := t.TempDir()
	fakeNet("192.168.1.20")
	m, _ := Open(dir)
	fp := m.Fingerprint()
	c1, _ := m.leafFor(nil)
	if c2, _ := m.leafFor(nil); c2 != c1 {
		t.Error("certificate should be reused while addresses are unchanged")
	}
	fakeNet("192.168.1.50") // DHCP gave a new address
	c3, _ := m.leafFor(nil)
	if c3 == c1 || parse(t, c3).VerifyHostname("192.168.1.50") != nil {
		t.Error("a new certificate covering the new address should be issued automatically")
	}
	again, _ := Open(dir)
	if again.Fingerprint() != fp || len(fp) != 95 {
		t.Errorf("CA must persist across restarts (fingerprint %q)", fp)
	}
	pem := string(m.CAPEM())
	if strings.Contains(pem, "PRIVATE") {
		t.Error("CAPEM must never contain the private key")
	}
}

func TestRealHandshakeTLS13Only(t *testing.T) {
	defer func() { ifaceAddrs = net.InterfaceAddrs }()
	fakeNet("192.168.1.20")
	m, _ := Open(t.TempDir())
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure hello") }))
	srv.TLS = m.TLSConfig()
	srv.StartTLS()
	defer srv.Close()

	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool(m)}}}
	addr := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	resp, err := trusting.Get(addr)
	if err != nil {
		t.Fatalf("trusted client failed: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "secure hello" || resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("body %q, TLS version %x", b, resp.TLS.Version)
	}
	// An untrusting client (no CA installed) must be refused.
	if _, err := http.Get(srv.URL); err == nil {
		t.Error("a client that does not trust the CA must fail")
	}
	// A TLS 1.2-only client must be refused.
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool(m), MaxVersion: tls.VersionTLS12}}}
	if _, err := old.Get(addr); err == nil {
		t.Error("TLS 1.2 must be rejected")
	}
}
