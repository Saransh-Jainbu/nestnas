package server

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Saransh-Jainbu/nestnas/internal/tlsutil"
)

func TestProfileEmbedsTheCA(t *testing.T) {
	m, err := tlsutil.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, "t", 0)
	s.EnableTLS(8443, m.CAPEM(), m.Fingerprint())

	rec := httptest.NewRecorder()
	s.serveProfile(rec, httptest.NewRequest("GET", "/NestNAS.mobileconfig", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-apple-aspen-config" {
		t.Errorf("content type %q", ct)
	}
	if rec.Header().Get("Content-Disposition") != "" {
		t.Error("iOS Safari needs the profile served inline, not as an attachment")
	}
	body := rec.Body.String()
	// well-formed XML, with the root-certificate payload type
	dec := xml.NewDecoder(strings.NewReader(body))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	for {
		if _, err := dec.Token(); err != nil {
			if err != io.EOF {
				t.Fatalf("profile is not well-formed XML: %v", err)
			}
			break
		}
	}
	if !strings.Contains(body, "com.apple.security.root") {
		t.Error("missing root certificate payload type")
	}
	i := strings.Index(body, "<data>")
	j := strings.Index(body, "</data>")
	der, err := base64.StdEncoding.DecodeString(body[i+6 : j])
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil || !cert.IsCA || cert.Subject.CommonName != "NestNAS Local CA" {
		t.Fatalf("embedded certificate is wrong: %v", err)
	}
	if strings.Contains(body, "PRIVATE") {
		t.Error("a private key must never appear in the profile")
	}
	// stable ids: installing again updates rather than duplicates
	rec2 := httptest.NewRecorder()
	s.serveProfile(rec2, httptest.NewRequest("GET", "/NestNAS.mobileconfig", nil))
	if rec2.Body.String() != body {
		t.Error("profile must be identical on every download (stable UUIDs)")
	}
	if u := uuidFrom(der, "cert"); len(u) != 36 || u == uuidFrom(der, "profile") {
		t.Errorf("bad uuids: %q", u)
	}

	d := httptest.NewRecorder()
	s.serveCADER(d, httptest.NewRequest("GET", "/ca.cer", nil))
	if _, err := x509.ParseCertificate(d.Body.Bytes()); err != nil {
		t.Errorf("DER download is not a certificate: %v", err)
	}
	p := httptest.NewRecorder()
	s.serveCA(p, httptest.NewRequest("GET", "/ca.crt", nil))
	if p.Header().Get("Content-Disposition") != "" || !strings.Contains(p.Body.String(), "BEGIN CERTIFICATE") {
		t.Error("PEM should be served inline")
	}
}

func TestDeviceSetupServesOnlyPublicMaterial(t *testing.T) {
	m, _ := tlsutil.Open(t.TempDir())
	s := New(nil, "t", 0)
	s.EnableTLS(8443, m.CAPEM(), m.Fingerprint())
	h := s.SetupHandler()
	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if r := get("GET", "/"); r.Code != 200 || !strings.Contains(r.Body.String(), m.Fingerprint()) || !strings.Contains(r.Body.String(), "NestNAS.mobileconfig") {
		t.Errorf("landing page: %d", r.Code)
	}
	for _, ok := range []string{"/NestNAS.mobileconfig", "/ca.crt", "/ca.cer"} {
		if r := get("GET", ok); r.Code != 200 {
			t.Errorf("%s -> %d", ok, r.Code)
		}
	}
	// nothing private is reachable from this plain-HTTP port
	for _, secret := range []string{"/api/status", "/api/login", "/api/setup", "/api/setup/browse", "/dav/", "/s/abc", "/sites/x/", "/api/download?path=/x", "/index.html"} {
		if r := get("GET", secret); r.Code != 404 {
			t.Errorf("%s must not be reachable on the device-setup port (got %d)", secret, r.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PROPFIND"} {
		if r := get(method, "/"); r.Code != 405 {
			t.Errorf("%s should be rejected, got %d", method, r.Code)
		}
	}
	if csp := get("GET", "/").Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("landing page needs a strict CSP, got %q", csp)
	}
}
