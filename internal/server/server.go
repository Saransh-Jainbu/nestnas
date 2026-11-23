// Package server implements NestNAS's HTTP API and web UI hosting.
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/auth"
	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/vault"
	"github.com/Saransh-Jainbu/nestnas/internal/web"
)

const (
	cookieName = "nestnas_session"
	sessionTTL = 7 * 24 * time.Hour
	maxJSON    = 1 << 20
)

type session struct {
	user    string
	expires time.Time
}

type usageEntry struct {
	bytes int64
	at    time.Time
}

type Server struct {
	st      *config.Store
	Version string
	Port    int

	mu          sync.Mutex
	sessions    map[string]session
	fails       map[string][]time.Time // ip -> recent failed logins
	usage       map[string]usageEntry
	davKey      []byte
	davCache    map[string]davEntry
	vault       *vault.Vault
	conv        converter
	mon         monitor
	convRunning int32
	thumbs      *thumbCache
	thumbSem    chan struct{}
	home        string     // NestNAS settings folder (auto-unlock key lives here)
	autoAt      time.Time  // last auto-unlock attempt
	TLSPort     int        // 0 = HTTPS disabled
	SetupPort   int        // 0 = device-setup page disabled
	caPEM       []byte     // public CA certificate served at /ca.crt
	caPrint     string     // its SHA-256 fingerprint
	idMu        sync.Mutex // serialises stamping a storage identity
	okPath      string     // storage path whose identity marker was verified recently
	okAt        time.Time  // when it was verified
	relocAt     time.Time  // last time we scanned for the drive at a new location

	Mux *http.ServeMux
}

func New(st *config.Store, version string, port int) *Server {
	s := &Server{st: st, Version: version, Port: port,
		sessions: map[string]session{}, fails: map[string][]time.Time{}, usage: map[string]usageEntry{},
		davKey: newDavKey(), davCache: map[string]davEntry{}, vault: vault.New(),
		thumbs: newThumbCache(48 << 20), thumbSem: make(chan struct{}, 3)}
	m := http.NewServeMux()
	s.Mux = m
	go s.snapshotLoop()
	go s.monitorLoop()

	m.HandleFunc("/", s.index)
	m.HandleFunc("/sites/", s.site)
	m.HandleFunc("/s/", s.publicShare)
	m.HandleFunc("/dav", s.davRoutes())
	m.HandleFunc("/dav/", s.davRoutes())

	m.HandleFunc("/api/status", s.status)
	m.HandleFunc("/api/setup", s.setup)
	m.HandleFunc("/api/login", s.login)
	m.HandleFunc("/api/setup/browse", s.browse)
	m.HandleFunc("/api/setup/mkdir", s.browseMkdir)
	m.HandleFunc("/api/logout", s.logout)

	m.HandleFunc("/api/me", s.authed(s.me))
	m.HandleFunc("/api/password", s.authed(s.changePassword))
	m.HandleFunc("/api/list", s.authed(s.list))
	m.HandleFunc("/api/download", s.authed(s.download))
	m.HandleFunc("/api/upload", s.authed(s.upload))
	m.HandleFunc("/api/view", s.authed(s.view))
	m.HandleFunc("/api/thumb", s.authed(s.thumb))
	m.HandleFunc("/api/backup/check", s.authed(s.backupCheck))
	m.HandleFunc("/api/zip", s.authed(s.zipFolder))
	m.HandleFunc("/api/monitor", s.adminOnly(s.monitorAPI))
	m.HandleFunc("/api/versions", s.authed(s.versionsAPI))
	m.HandleFunc("/api/versions/download", s.authed(s.versionDownload))
	m.HandleFunc("/api/versions/restore", s.authed(s.versionRestore))
	m.HandleFunc("/api/versions/delete", s.authed(s.versionDelete))
	m.HandleFunc("/api/snapshots", s.authed(s.snapshotsList))
	m.HandleFunc("/api/snapshots/create", s.authed(s.snapshotCreate))
	m.HandleFunc("/api/snapshots/delete", s.authed(s.snapshotDelete))
	m.HandleFunc("/api/snapshots/browse", s.authed(s.snapshotBrowse))
	m.HandleFunc("/api/snapshots/download", s.authed(s.snapshotDownload))
	m.HandleFunc("/api/snapshots/restore", s.authed(s.snapshotRestore))
	m.HandleFunc("/api/snapshots/auto", s.adminOnly(s.snapshotAuto))
	m.HandleFunc("/api/trash", s.authed(s.trashList))
	m.HandleFunc("/api/trash/restore", s.authed(s.trashRestore))
	m.HandleFunc("/api/trash/delete", s.authed(s.trashDelete))
	m.HandleFunc("/api/trash/empty", s.authed(s.trashEmpty))
	m.HandleFunc("/api/mkdir", s.authed(s.mkdir))
	m.HandleFunc("/api/delete", s.authed(s.remove))
	m.HandleFunc("/api/rename", s.authed(s.rename))
	m.HandleFunc("/api/shares", s.authed(s.listShares))
	m.HandleFunc("/api/share", s.authed(s.createShare))
	m.HandleFunc("/api/share/delete", s.authed(s.deleteShare))

	m.Handle("/fonts/", s.fonts())
	m.HandleFunc("/manifest.webmanifest", s.manifest)
	m.HandleFunc("/icon.png", s.icon)
	m.HandleFunc("/icon-512.png", s.icon512)
	m.HandleFunc("/favicon.svg", s.favicon)
	m.HandleFunc("/apple-touch-icon.png", s.icon)
	m.HandleFunc("/ca.crt", s.serveCA)
	m.HandleFunc("/ca.cer", s.serveCADER)
	m.HandleFunc("/NestNAS.mobileconfig", s.serveProfile)
	m.HandleFunc("/api/vault", s.authed(s.vaultInfo))
	m.HandleFunc("/api/vault/unlock", s.adminOnly(s.vaultUnlock))
	m.HandleFunc("/api/vault/lock", s.adminOnly(s.vaultLock))
	m.HandleFunc("/api/vault/convert", s.adminOnly(s.vaultConvert))
	m.HandleFunc("/api/vault/passphrase", s.adminOnly(s.vaultPassphrase))
	m.HandleFunc("/api/vault/autounlock", s.adminOnly(s.vaultAutoUnlock))
	m.HandleFunc("/api/system", s.adminOnly(s.system))
	m.HandleFunc("/api/users", s.adminOnly(s.users))
	m.HandleFunc("/api/users/add", s.adminOnly(s.addUser))
	m.HandleFunc("/api/users/delete", s.adminOnly(s.deleteUser))
	m.HandleFunc("/api/users/quota", s.adminOnly(s.setQuota))
	m.HandleFunc("/api/apps", s.adminOnly(s.apps))
	m.HandleFunc("/api/apps/install", s.adminOnly(s.appInstall))
	m.HandleFunc("/api/apps/remove", s.adminOnly(s.appRemove))
	return s
}

// Handler wraps the mux with security headers and CSRF protection.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Cross-site request forgery defence: a browser-sent Origin must match our Host.
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					jerr(w, 403, "bad origin")
					return
				}
			}
		}
		if r.URL.Path == "/api/monitor" { // the monitor's own polling is not traffic
			s.Mux.ServeHTTP(w, r)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = countReader{r.Body, &s.mon.in}
		}
		s.Mux.ServeHTTP(countWriter{w, &s.mon.out}, r)
	})
}

// ---------- helpers ----------

func jsonOut(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jerr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSON)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jerr(w, 400, "bad request")
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

var errNoStorage = errors.New("storage drive not connected")

const markerName = ".nestnas-id"

// placesFn lists mounted drives; a variable so tests can simulate re-plugging a drive.
var placesFn = listPlaces

// storageOK reports whether the storage drive is present AND is the right one. An unplugged
// external drive leaves an empty mount point on the internal disk, and without this check
// NestNAS would quietly fill the laptop's disk (or serve a different drive's files). If the
// drive was mounted somewhere else this time, it is found again by its identity marker.
func (s *Server) storageOK() bool {
	if s.driveHere(s.st.StoragePath()) {
		return true
	}
	s.mu.Lock() // not here: look for the drive at a new mount point, at most every 2s
	recent := time.Since(s.relocAt) < 2*time.Second
	if !recent {
		s.relocAt = time.Now()
	}
	s.mu.Unlock()
	if recent || !s.tryRelocate() {
		return false
	}
	return s.driveHere(s.st.StoragePath())
}

// driveHere checks, freshly on every call, that the storage folder exists (so an unplug is
// noticed immediately), and that it carries our identity marker (cached for 2s).
func (s *Server) driveHere(p string) bool {
	if fi, err := os.Stat(filepath.Join(p, "users")); err != nil || !fi.IsDir() {
		return false
	}
	id := s.st.StorageID()
	if id == "" { // storage from an older version: adopt it by stamping an identity now
		s.initStorageIdentity(p)
		return true
	}
	s.mu.Lock()
	cached := s.okPath == p && time.Since(s.okAt) < 2*time.Second
	s.mu.Unlock()
	if cached {
		return true
	}
	b, err := os.ReadFile(filepath.Join(p, markerName))
	if err != nil || strings.TrimSpace(string(b)) != id {
		return false
	}
	s.mu.Lock()
	s.okPath, s.okAt = p, time.Now()
	s.mu.Unlock()
	return true
}

// tryRelocate looks for our drive at a new mount point (same folder inside the drive, same marker).
func (s *Server) tryRelocate() bool {
	id, sub := s.st.StorageID(), s.st.StorageSub()
	if id == "" || sub == "" {
		return false
	}
	for _, p := range placesFn() {
		cand := filepath.Join(p.Path, sub)
		if b, err := os.ReadFile(filepath.Join(cand, markerName)); err == nil && strings.TrimSpace(string(b)) == id {
			if err := s.st.SetStoragePath(cand); err != nil {
				return false
			}
			s.mu.Lock()
			s.usage = map[string]usageEntry{}
			s.mu.Unlock()
			log.Printf("storage drive found again at %s", cand)
			return true
		}
	}
	return false
}

// initStorageIdentity stamps the storage folder with a random id and remembers where it sits
// inside its drive, so it can be recognised after the drive is unplugged and plugged back in.
func (s *Server) initStorageIdentity(storage string) {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if s.st.StorageID() != "" {
		return // already stamped (e.g. by a concurrent request)
	}
	id := auth.Token(16)
	if os.WriteFile(filepath.Join(storage, markerName), []byte(id+"\n"), 0o644) != nil {
		return
	}
	sub, best := "", -1
	for _, p := range placesFn() {
		rel, err := filepath.Rel(p.Path, storage)
		if err == nil && !strings.HasPrefix(rel, "..") && len(p.Path) > best {
			sub, best = rel, len(p.Path)
		}
	}
	_ = s.st.SetStorageIdentity(id, sub)
}

const storageGoneMsg = "Your storage drive isn't connected. Plug it back in and try again."

func (s *Server) userRoot(name string) (string, error) {
	if !s.storageOK() {
		return "", errNoStorage
	}
	root := filepath.Join(s.st.StoragePath(), "users", name)
	// Mkdir, never MkdirAll: if the drive vanished a moment ago this must fail rather than
	// recreate the storage folders on the internal disk.
	if err := os.Mkdir(root, 0o755); err != nil && !os.IsExist(err) {
		if os.IsNotExist(err) {
			return "", errNoStorage
		}
		return "", err
	}
	return root, nil
}

func (s *Server) usedBytes(user string, root string, fresh bool) int64 {
	s.mu.Lock()
	e, ok := s.usage[user]
	s.mu.Unlock()
	if ok && !fresh && time.Since(e.at) < 15*time.Second {
		return e.bytes
	}
	n := dirSize(root)
	s.mu.Lock()
	s.usage[user] = usageEntry{n, time.Now()}
	s.mu.Unlock()
	return n
}

func (s *Server) invalidate(user string) {
	s.mu.Lock()
	delete(s.usage, user)
	s.mu.Unlock()
}

// ---------- sessions ----------

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user string, extra map[string]interface{}) {
	tok := auth.Token(32)
	s.mu.Lock()
	for k, v := range s.sessions { // opportunistic cleanup
		if time.Now().After(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[tok] = session{user, time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds())})
	out := map[string]interface{}{"ok": true}
	for k, v := range extra {
		out[k] = v
	}
	jsonOut(w, 200, out)
}

func (s *Server) currentUser(r *http.Request) (config.User, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return config.User{}, false
	}
	s.mu.Lock()
	sess, ok := s.sessions[c.Value]
	if ok && time.Now().After(sess.expires) {
		delete(s.sessions, c.Value)
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		return config.User{}, false
	}
	return s.st.User(sess.user) // fails if the user was deleted
}

type userHandler func(w http.ResponseWriter, r *http.Request, u config.User)

func (s *Server) authed(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			jerr(w, 401, "login required")
			return
		}
		if !s.storageOK() {
			jerr(w, 503, storageGoneMsg)
			return
		}
		if !lockExempt(r.URL.Path) && s.locked() {
			jerr(w, http.StatusLocked, lockedMsg)
			return
		}
		h(w, r, u)
	}
}

func (s *Server) adminOnly(h userHandler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, u config.User) {
		if !u.Admin {
			jerr(w, 403, "admins only")
			return
		}
		h(w, r, u)
	})
}

func (s *Server) tooManyFails(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := s.fails[ip][:0]
	for _, t := range s.fails[ip] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.fails[ip] = recent
	return len(recent) >= 8
}

// ---------- pages & auth endpoints ----------

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
	_, _ = w.Write(web.Index)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	_, logged := s.currentUser(r)
	jsonOut(w, 200, map[string]interface{}{"setup_needed": !s.st.Configured(), "logged_in": logged, "version": s.Version,
		"storage_ok": !s.st.Configured() || s.storageOK(),
		"encrypted":  s.encrypted(), "locked": s.st.Configured() && s.storageOK() && s.locked(), "tls": s.TLSPort > 0})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jerr(w, 405, "POST only")
		return
	}
	if s.st.Configured() {
		jerr(w, 409, "already set up")
		return
	}
	var d struct {
		Username    string  `json:"username"`
		Password    string  `json:"password"`
		StoragePath string  `json:"storage_path"`
		QuotaGB     float64 `json:"quota_gb"`
		Encrypt     bool    `json:"encrypt"`
		Passphrase  string  `json:"passphrase"`
		AutoUnlock  bool    `json:"auto_unlock"`
	}
	if !readJSON(w, r, &d) {
		return
	}
	d.Username = strings.ToLower(strings.TrimSpace(d.Username))
	if !config.ValidName(d.Username) {
		jerr(w, 400, "Username: 1-32 chars, a-z 0-9 _ -")
		return
	}
	if len(d.Password) < 8 {
		jerr(w, 400, "Password must be at least 8 characters")
		return
	}
	if d.QuotaGB <= 0 {
		jerr(w, 400, "Size must be greater than 0")
		return
	}
	if d.Encrypt {
		if len(d.Passphrase) < 12 {
			jerr(w, 400, "Encryption passphrase must be at least 12 characters (a few random words work well)")
			return
		}
		if d.Passphrase == d.Password {
			jerr(w, 400, "Use a different passphrase for encryption than your login password")
			return
		}
	}
	storage := strings.TrimSpace(d.StoragePath)
	if storage == "" {
		home, _ := os.UserHomeDir()
		storage = filepath.Join(home, "NestNAS-Storage")
	} else if strings.HasPrefix(storage, "~/") {
		home, _ := os.UserHomeDir()
		storage = filepath.Join(home, storage[2:])
	}
	storage, err := filepath.Abs(storage)
	if err == nil {
		err = os.MkdirAll(filepath.Join(storage, "users"), 0o755)
	}
	if err != nil {
		jerr(w, 400, "Can't create storage folder: "+err.Error())
		return
	}
	recovery := ""
	if d.Encrypt {
		rec, err := s.vault.Init(storage, d.Passphrase)
		if errors.Is(err, vault.ErrExists) {
			jerr(w, 409, "That folder already holds encrypted NestNAS data. Choose a new, empty folder.")
			return
		} else if err != nil {
			jerr(w, 500, "couldn't set up encryption")
			return
		}
		recovery = rec
	}
	if err := s.st.Setup(d.Username, d.Password, storage, int64(d.QuotaGB*float64(config.GB))); err != nil {
		if d.Encrypt {
			s.vault.Lock()
			os.Remove(filepath.Join(storage, vault.KeyFile))
		}
		jerr(w, 500, "setup failed")
		return
	}
	var extra map[string]interface{}
	if d.Encrypt {
		_ = s.st.SetEncryption(true, d.AutoUnlock)
		if d.AutoUnlock {
			_ = s.writeAutoKey()
		}
		extra = map[string]interface{}{"recovery_key": recovery}
	}
	s.initStorageIdentity(storage)
	s.startSession(w, r, d.Username, extra)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jerr(w, 405, "POST only")
		return
	}
	ip := clientIP(r)
	if s.tooManyFails(ip) {
		jerr(w, 429, "Too many attempts. Try again in a few minutes.")
		return
	}
	var d struct{ Username, Password string }
	if !readJSON(w, r, &d) {
		return
	}
	u, ok := s.st.Authenticate(strings.ToLower(strings.TrimSpace(d.Username)), d.Password)
	if !ok {
		s.mu.Lock()
		s.fails[ip] = append(s.fails[ip], time.Now())
		s.mu.Unlock()
		time.Sleep(time.Second)
		jerr(w, 401, "Wrong username or password")
		return
	}
	s.startSession(w, r, u.Name, nil)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	jsonOut(w, 200, map[string]interface{}{
		"name": u.Name, "admin": u.Admin, "quota": u.QuotaBytes, "used": s.usedBytes(u.Name, root, false),
	})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Old, New string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if _, ok := s.st.Authenticate(u.Name, d.Old); !ok {
		jerr(w, 403, "Current password is wrong")
		return
	}
	if err := s.st.SetPassword(u.Name, d.New); err != nil {
		jerr(w, 400, "New password must be at least 8 characters")
		return
	}
	s.clearDavCache()
	jsonOut(w, 200, map[string]bool{"ok": true})
}
