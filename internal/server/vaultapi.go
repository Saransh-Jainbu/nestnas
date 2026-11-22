package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/vault"
)

// lockExempt lists the API calls that still work while encrypted storage is locked: everything
// that does not touch file contents, so an admin can log in and unlock.
func lockExempt(p string) bool {
	switch p {
	case "/api/me", "/api/list", "/api/system", "/api/password", "/api/shares", "/api/share/delete":
		return true
	}
	return strings.HasPrefix(p, "/api/vault") || strings.HasPrefix(p, "/api/users") || strings.HasPrefix(p, "/api/apps")
}

func (s *Server) writeAutoKey() error {
	k, err := s.vault.ExportKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.autoKeyPath(), []byte(hex.EncodeToString(k)), 0o600)
}

func (s *Server) vaultInfo(w http.ResponseWriter, r *http.Request, u config.User) {
	s.resumeConversion() // picks up a conversion that a restart interrupted
	jsonOut(w, 200, map[string]interface{}{
		"enabled": s.encrypted(), "locked": s.locked(), "auto_unlock": s.st.AutoUnlock(),
		"convert": s.conv.get(), "pending": s.st.Converting(),
	})
}

func (s *Server) vaultUnlock(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Secret string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if !s.encrypted() {
		jerr(w, 400, "Storage encryption isn't turned on")
		return
	}
	ip := clientIP(r)
	if s.tooManyFails(ip) {
		jerr(w, 429, "Too many attempts. Try again in a few minutes.")
		return
	}
	if err := s.vault.Unlock(s.st.StoragePath(), d.Secret); err != nil {
		s.mu.Lock()
		s.fails[ip] = append(s.fails[ip], time.Now())
		s.mu.Unlock()
		time.Sleep(time.Second)
		jerr(w, 401, "Wrong passphrase or recovery key")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) vaultLock(w http.ResponseWriter, r *http.Request, u config.User) {
	if r.Method != http.MethodPost {
		return
	}
	s.vault.Lock()
	s.mu.Lock()
	s.autoAt = time.Now().Add(time.Hour) // a manual lock must stick: no auto-unlock until restart
	s.mu.Unlock()
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) vaultPassphrase(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Old, New string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if len(d.New) < 12 {
		jerr(w, 400, "New passphrase must be at least 12 characters")
		return
	}
	if err := s.vault.ChangePassphrase(s.st.StoragePath(), d.Old, d.New); err != nil {
		if err == vault.ErrBadSecret {
			jerr(w, 403, "Current passphrase is wrong")
		} else {
			jerr(w, 500, "couldn't change the passphrase")
		}
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) vaultAutoUnlock(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct {
		Enable bool
		Secret string
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if !s.encrypted() {
		jerr(w, 400, "Storage encryption isn't turned on")
		return
	}
	if d.Enable {
		if err := vault.New().Unlock(s.st.StoragePath(), d.Secret); err != nil { // re-check the passphrase
			jerr(w, 403, "Wrong passphrase")
			return
		}
		if err := s.writeAutoKey(); err != nil {
			jerr(w, 500, "couldn't save the key on this computer (is the storage unlocked?)")
			return
		}
	} else {
		_ = os.Remove(s.autoKeyPath())
	}
	_ = s.st.SetEncryption(true, d.Enable)
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// caDER returns the CA certificate in binary (DER) form.
func (s *Server) caDER() []byte {
	blk, _ := pem.Decode(s.caPEM)
	if blk == nil {
		return nil
	}
	return blk.Bytes
}

// uuidFrom makes a stable UUID-shaped id from the certificate, so installing the profile again
// updates it instead of creating a duplicate.
func uuidFrom(der []byte, salt string) string {
	sum := sha256.Sum256(append([]byte(salt), der...))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// serveCA lets a device download the public CA certificate (PEM, shown inline so iOS Safari
// offers to install it rather than saving a file).
func (s *Server) serveCA(w http.ResponseWriter, r *http.Request) {
	if len(s.caPEM) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.caPEM)
}

// serveCADER is the same certificate in binary DER form (what iOS and Windows prefer).
func (s *Server) serveCADER(w http.ResponseWriter, r *http.Request) {
	der := s.caDER()
	if der == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(der)
}

// serveProfile serves an Apple configuration profile that installs the CA on iPhone/iPad/Mac.
// This is the most reliable way to install a certificate on iOS.
func (s *Server) serveProfile(w http.ResponseWriter, r *http.Request) {
	der := s.caDER()
	if der == nil {
		http.NotFound(w, r)
		return
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
		<dict>
			<key>PayloadCertificateFileName</key><string>NestNAS-CA.cer</string>
			<key>PayloadContent</key><data>%s</data>
			<key>PayloadDescription</key><string>Lets this device trust your NestNAS over HTTPS</string>
			<key>PayloadDisplayName</key><string>NestNAS Local CA</string>
			<key>PayloadIdentifier</key><string>io.nestnas.ca.certificate</string>
			<key>PayloadType</key><string>com.apple.security.root</string>
			<key>PayloadUUID</key><string>%s</string>
			<key>PayloadVersion</key><integer>1</integer>
		</dict>
	</array>
	<key>PayloadDescription</key><string>Trust your own NestNAS (private network addresses only)</string>
	<key>PayloadDisplayName</key><string>NestNAS</string>
	<key>PayloadIdentifier</key><string>io.nestnas.ca</string>
	<key>PayloadOrganization</key><string>NestNAS</string>
	<key>PayloadRemovalDisallowed</key><false/>
	<key>PayloadType</key><string>Configuration</string>
	<key>PayloadUUID</key><string>%s</string>
	<key>PayloadVersion</key><integer>1</integer>
</dict>
</plist>
`, base64.StdEncoding.EncodeToString(der), uuidFrom(der, "cert"), uuidFrom(der, "profile"))
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// EnableTLS tells the server it is reachable over HTTPS on port, with the given public CA.
func (s *Server) EnableTLS(port int, caPEM []byte, fingerprint string) {
	s.TLSPort, s.caPEM, s.caPrint = port, caPEM, fingerprint
}

// SetHome tells the server where NestNAS keeps its settings.
func (s *Server) SetHome(home string) { s.home = home }
