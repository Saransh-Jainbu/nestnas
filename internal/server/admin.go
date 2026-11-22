package server

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

type userView struct {
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
	Quota int64  `json:"quota"`
	Used  int64  `json:"used"`
}

func (s *Server) users(w http.ResponseWriter, r *http.Request, _ config.User) {
	out := []userView{}
	for _, u := range s.st.Users() {
		root, _ := s.userRoot(u.Name)
		out = append(out, userView{u.Name, u.Admin, u.QuotaBytes, s.usedBytes(u.Name, root, false)})
	}
	jsonOut(w, 200, map[string]interface{}{"users": out})
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request, _ config.User) {
	var d struct {
		Name     string  `json:"name"`
		Password string  `json:"password"`
		QuotaGB  float64 `json:"quota_gb"`
		Admin    bool    `json:"admin"`
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	d.Name = strings.ToLower(strings.TrimSpace(d.Name))
	switch err := s.st.AddUser(d.Name, d.Password, int64(d.QuotaGB*float64(config.GB)), d.Admin); err {
	case nil:
		_, _ = s.userRoot(d.Name)
		jsonOut(w, 200, map[string]bool{"ok": true})
	case config.ErrExists:
		jerr(w, 409, "That username is taken")
	default:
		jerr(w, 400, "Username: a-z 0-9 _ - (max 32). Password: 8+ chars. Size: more than 0 GB.")
	}
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, me config.User) {
	var d struct {
		Name       string `json:"name"`
		DeleteData bool   `json:"delete_data"`
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if d.Name == me.Name {
		jerr(w, 400, "You can't delete yourself")
		return
	}
	if err := s.st.DeleteUser(d.Name); err != nil {
		jerr(w, 400, "Can't delete that user")
		return
	}
	s.clearDavCache()
	s.mu.Lock()
	for k, v := range s.sessions {
		if v.user == d.Name {
			delete(s.sessions, k)
		}
	}
	s.mu.Unlock()
	if d.DeleteData && config.ValidName(d.Name) {
		_ = os.RemoveAll(filepath.Join(s.st.StoragePath(), "users", d.Name))
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) setQuota(w http.ResponseWriter, r *http.Request, _ config.User) {
	var d struct {
		Name    string  `json:"name"`
		QuotaGB float64 `json:"quota_gb"`
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if err := s.st.SetQuota(d.Name, int64(d.QuotaGB*float64(config.GB))); err != nil {
		jerr(w, 400, "Invalid size or user")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// ---------- system / connectivity ----------

func LANURLs(port int) []string { return LANURLsScheme("http", port) }

// LANURLsScheme lists the addresses other devices on the network can use.
func LANURLsScheme(scheme string, port int) []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		if ipn.IP.IsPrivate() || ipn.IP.IsGlobalUnicast() {
			out = append(out, scheme+"://"+net.JoinHostPort(ipn.IP.String(), itoa(port)))
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

func (s *Server) system(w http.ResponseWriter, r *http.Request, u config.User) {
	storage := s.st.StoragePath()
	free, _ := diskFree(storage)
	total, _ := diskTotal(storage)
	var allotted int64
	for _, x := range s.st.Users() {
		allotted += x.QuotaBytes
	}
	hn, _ := os.Hostname()
	jsonOut(w, 200, map[string]interface{}{
		"version":      s.Version,
		"hostname":     hn,
		"urls":         s.deviceURLs(),
		"tls":          map[string]interface{}{"enabled": s.TLSPort > 0, "port": s.TLSPort, "fingerprint": s.caPrint, "setup_urls": s.setupURLs()},
		"encryption":   map[string]interface{}{"enabled": s.encrypted(), "locked": s.locked(), "auto_unlock": s.st.AutoUnlock()},
		"storage_path": storage,
		"disk_free":    free,
		"disk_total":   total,
		"allotted":     allotted,
		"remote":       remoteStatus(),
		"docker":       dockerStatus(),
	})
}

// deviceURLs are the addresses other devices should use: HTTPS when it is available.
func (s *Server) deviceURLs() []string {
	if s.TLSPort > 0 {
		return LANURLsScheme("https", s.TLSPort)
	}
	return LANURLs(s.Port)
}

// setupURLs are the plain-HTTP device-setup addresses (public certificate only).
func (s *Server) setupURLs() []string {
	if s.SetupPort <= 0 {
		return []string{}
	}
	return LANURLsScheme("http", s.SetupPort)
}
