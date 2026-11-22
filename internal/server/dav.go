package server

// WebDAV network drive, implemented in-house (standard library only) at /dav/.
//
// Works as a mountable drive in the iPhone/iPad Files app, macOS Finder, Linux file managers,
// and Windows (see README for Windows notes). Every user logs in with their NestNAS username
// and password and sees only their own folder. Quotas, the path jail and the trash all apply.
//
// Supported: OPTIONS, GET/HEAD (with Range), PUT, MKCOL, DELETE (-> trash), MOVE, COPY,
// PROPFIND (Depth 0/1), PROPPATCH (accepted, ignored), and a minimal LOCK/UNLOCK that Finder
// needs before it will write. Locks are not enforced.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/auth"
	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/fsx"
)

const (
	davPrefix   = "/dav"
	davCacheTTL = 5 * time.Minute
)

type davEntry struct {
	user string
	exp  time.Time
}

// davLogin checks Basic credentials. PBKDF2 is deliberately slow, and file managers send
// credentials on every request, so successful logins are cached briefly in memory.
func (s *Server) davLogin(name, pass string) (config.User, bool) {
	mac := hmac.New(sha256.New, s.davKey)
	mac.Write([]byte(name + "\x00" + pass))
	key := hex.EncodeToString(mac.Sum(nil))
	s.mu.Lock()
	e, ok := s.davCache[key]
	s.mu.Unlock()
	if ok && time.Now().Before(e.exp) {
		if u, ok := s.st.User(e.user); ok {
			return u, true
		}
	}
	u, ok := s.st.Authenticate(strings.ToLower(name), pass)
	if !ok {
		return config.User{}, false
	}
	s.mu.Lock()
	s.davCache[key] = davEntry{u.Name, time.Now().Add(davCacheTTL)}
	s.mu.Unlock()
	return u, true
}

// clearDavCache forgets cached logins (after a password change or user deletion).
func (s *Server) clearDavCache() {
	s.mu.Lock()
	s.davCache = map[string]davEntry{}
	s.mu.Unlock()
}

func newDavKey() []byte {
	k, _ := hex.DecodeString(auth.Token(32))
	return k
}

type davHandler func(w http.ResponseWriter, r *http.Request, u config.User, root, rel string)

func (s *Server) dav(h davHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("DAV", "1, 2")
		w.Header().Set("MS-Author-Via", "DAV")
		if r.Method == http.MethodOptions { // clients probe before logging in
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE, PROPFIND, PROPPATCH, LOCK, UNLOCK")
			w.WriteHeader(http.StatusOK)
			return
		}
		name, pass, hasCreds := r.BasicAuth()
		ip := clientIP(r)
		if hasCreds && s.tooManyFails(ip) {
			http.Error(w, "Too many attempts", http.StatusTooManyRequests)
			return
		}
		var u config.User
		ok := false
		if hasCreds {
			u, ok = s.davLogin(name, pass)
		}
		if !ok {
			if hasCreds {
				s.mu.Lock()
				s.fails[ip] = append(s.fails[ip], time.Now())
				s.mu.Unlock()
				time.Sleep(time.Second)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="NestNAS", charset="UTF-8"`)
			http.Error(w, "Login required", http.StatusUnauthorized)
			return
		}
		if s.locked() {
			http.Error(w, lockedMsg, http.StatusLocked)
			return
		}
		root, err := s.userRoot(u.Name)
		if err == errNoStorage {
			http.Error(w, storageGoneMsg, http.StatusServiceUnavailable)
			return
		} else if err != nil {
			http.Error(w, "storage error", 500)
			return
		}
		rel := path.Clean("/" + strings.TrimPrefix(r.URL.Path, davPrefix))
		h(w, r, u, root, rel)
	}
}

func (s *Server) davRoutes() http.HandlerFunc {
	handlers := map[string]davHandler{
		"GET": s.davGet, "HEAD": s.davGet, "PUT": s.davPut, "MKCOL": s.davMkcol, "DELETE": s.davDelete,
		"MOVE": s.davMove, "COPY": s.davCopy, "PROPFIND": s.davPropfind, "PROPPATCH": s.davProppatch,
		"LOCK": s.davLock, "UNLOCK": s.davUnlock,
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == davPrefix { // be forgiving about the missing slash
			http.Redirect(w, r, davPrefix+"/", http.StatusMovedPermanently)
			return
		}
		h, ok := handlers[r.Method]
		if !ok && r.Method != http.MethodOptions {
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE, PROPFIND, PROPPATCH, LOCK, UNLOCK")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.dav(h)(w, r)
	}
}

// davPath resolves a DAV-relative path inside the user's folder.
func davPath(root, rel string) (string, bool) {
	if inTrash(rel) {
		return "", false
	}
	abs, err := fsx.Resolve(root, rel)
	return abs, err == nil
}

func hrefFor(rel string, isDir bool) string {
	p := davPrefix + rel
	if isDir && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ---------- GET / HEAD ----------

func (s *Server) davGet(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	abs, ok := davPath(root, rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "This is a NestNAS network drive. Open it from your file manager (Files, Finder, Explorer).")
		return
	}
	rs, fi, done, err := s.openContent(abs)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			contentErr(w, err)
		}
		return
	}
	defer done()
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), rs)
}

// ---------- PROPFIND ----------

func (s *Server) davProps(w io.Writer, href string, fi os.FileInfo, size, quotaAvail, quotaUsed int64) {
	fmt.Fprintf(w, `<D:response><D:href>%s</D:href><D:propstat><D:prop>`, xmlText(href))
	fmt.Fprintf(w, `<D:displayname>%s</D:displayname>`, xmlText(fi.Name()))
	fmt.Fprintf(w, `<D:getlastmodified>%s</D:getlastmodified>`, fi.ModTime().UTC().Format(http.TimeFormat))
	fmt.Fprintf(w, `<D:creationdate>%s</D:creationdate>`, fi.ModTime().UTC().Format(time.RFC3339))
	if fi.IsDir() {
		fmt.Fprint(w, `<D:resourcetype><D:collection/></D:resourcetype>`)
		if quotaAvail >= 0 {
			fmt.Fprintf(w, `<D:quota-available-bytes>%d</D:quota-available-bytes><D:quota-used-bytes>%d</D:quota-used-bytes>`, quotaAvail, quotaUsed)
		}
	} else {
		fmt.Fprint(w, `<D:resourcetype/>`)
		fmt.Fprintf(w, `<D:getcontentlength>%d</D:getcontentlength>`, size)
		fmt.Fprintf(w, `<D:getcontenttype>%s</D:getcontenttype>`, xmlText(contentType(fi.Name())))
		fmt.Fprintf(w, `<D:getetag>"%x-%x"</D:getetag>`, fi.ModTime().UnixNano(), size)
	}
	fmt.Fprint(w, `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
}

func contentType(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (s *Server) davPropfind(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20)) // we always answer with the standard property set
	abs, ok := davPath(root, rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	used := s.usedBytes(u.Name, root, false)
	avail := u.QuotaBytes - used
	if avail < 0 {
		avail = 0
	}
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
	s.davProps(w, hrefFor(rel, fi.IsDir()), fi, s.plainSize(fi), avail, used)
	if fi.IsDir() && r.Header.Get("Depth") != "0" {
		des, _ := os.ReadDir(abs)
		for _, d := range des {
			n := d.Name()
			if abs == root && n == trashName || strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".part") {
				continue
			}
			info, err := d.Info()
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			s.davProps(w, hrefFor(path.Join(rel, n), info.IsDir()), info, s.plainSize(info), avail, used)
		}
	}
	fmt.Fprint(w, `</D:multistatus>`)
}

func (s *Server) davProppatch(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>%s</D:href><D:propstat><D:prop/><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`, xmlText(hrefFor(rel, false)))
}

// ---------- PUT ----------

func (s *Server) davPut(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	abs, ok := davPath(root, rel)
	if !ok || abs == root || !fsx.ValidName(filepath.Base(abs)) {
		http.Error(w, "bad path", http.StatusForbidden)
		return
	}
	if fi, err := os.Stat(filepath.Dir(abs)); err != nil || !fi.IsDir() {
		http.Error(w, "parent folder missing", http.StatusConflict)
		return
	}
	existing, err := os.Lstat(abs)
	existed := err == nil
	if existed && existing.IsDir() {
		http.Error(w, "that is a folder", http.StatusMethodNotAllowed)
		return
	}
	used := s.usedBytes(u.Name, root, true)
	if existed {
		used -= existing.Size() // overwriting frees the old copy
	}
	remaining := u.QuotaBytes - used
	if remaining <= 0 || (r.ContentLength > 0 && r.ContentLength > remaining) {
		http.Error(w, "Not enough space left in your NAS allowance", http.StatusInsufficientStorage)
		return
	}
	if free, ok := diskFree(root); ok && r.ContentLength > 0 && free < uint64(r.ContentLength)+uint64(config.GB/20) {
		http.Error(w, "The laptop's disk is nearly full", http.StatusInsufficientStorage)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".dav-*.part")
	if err != nil {
		http.Error(w, "can't write", 500)
		return
	}
	cw, err := s.newContentWriter(tmp)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		http.Error(w, lockedMsg, http.StatusLocked)
		return
	}
	n, err := io.Copy(cw, io.LimitReader(r.Body, remaining+1))
	if cerr := cw.Close(); err == nil {
		err = cerr
	}
	tmp.Close()
	if err != nil || n > remaining {
		os.Remove(tmp.Name())
		if err == nil {
			http.Error(w, "Not enough space left in your NAS allowance", http.StatusInsufficientStorage)
		} else {
			http.Error(w, "upload interrupted", http.StatusBadRequest)
		}
		return
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	if err := os.Rename(tmp.Name(), abs); err != nil {
		os.Remove(tmp.Name())
		http.Error(w, "can't save file", 500)
		return
	}
	s.invalidate(u.Name)
	if existed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

// ---------- MKCOL / DELETE ----------

func (s *Server) davMkcol(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	abs, ok := davPath(root, rel)
	if !ok || abs == root || !fsx.ValidName(filepath.Base(abs)) {
		http.Error(w, "bad path", http.StatusForbidden)
		return
	}
	if fi, err := os.Stat(filepath.Dir(abs)); err != nil || !fi.IsDir() {
		http.Error(w, "parent folder missing", http.StatusConflict)
		return
	}
	if _, err := os.Lstat(abs); err == nil {
		http.Error(w, "already exists", http.StatusMethodNotAllowed)
		return
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		http.Error(w, "can't create folder", 500)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) davDelete(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	abs, ok := davPath(root, rel)
	if !ok || abs == root {
		http.Error(w, "can't delete that", http.StatusForbidden)
		return
	}
	if _, err := os.Lstat(abs); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := moveToTrash(root, abs, rel); err != nil {
		http.Error(w, "delete failed", 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- MOVE / COPY ----------

// davDest parses the Destination header into a DAV-relative path.
func davDest(r *http.Request) (string, bool) {
	d := r.Header.Get("Destination")
	if d == "" {
		return "", false
	}
	u, err := url.Parse(d)
	if err != nil || (u.Host != "" && u.Host != r.Host) || !strings.HasPrefix(u.Path, davPrefix+"/") && u.Path != davPrefix {
		return "", false
	}
	return path.Clean("/" + strings.TrimPrefix(u.Path, davPrefix)), true
}

// davTarget validates a MOVE/COPY and clears the way for the destination.
// Returns src, dst absolute paths and whether the destination already existed.
func (s *Server) davTarget(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) (src, dst string, existed, ok bool) {
	srcAbs, ok1 := davPath(root, rel)
	drel, ok2 := davDest(r)
	if !ok1 || !ok2 {
		http.Error(w, "bad source or destination", http.StatusBadRequest)
		return
	}
	dstAbs, ok3 := davPath(root, drel)
	if !ok3 || srcAbs == root || dstAbs == root || !fsx.ValidName(filepath.Base(dstAbs)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, err := os.Lstat(srcAbs); err != nil {
		http.NotFound(w, r)
		return
	}
	if dstAbs == srcAbs || strings.HasPrefix(dstAbs, srcAbs+string(filepath.Separator)) {
		http.Error(w, "can't move a folder into itself", http.StatusForbidden)
		return
	}
	if fi, err := os.Stat(filepath.Dir(dstAbs)); err != nil || !fi.IsDir() {
		http.Error(w, "destination folder missing", http.StatusConflict)
		return
	}
	if _, err := os.Lstat(dstAbs); err == nil {
		existed = true
		if strings.EqualFold(r.Header.Get("Overwrite"), "F") {
			http.Error(w, "destination exists", http.StatusPreconditionFailed)
			return
		}
		if err := moveToTrash(root, dstAbs, drel); err != nil { // overwritten items go to the trash
			http.Error(w, "can't replace destination", 500)
			return
		}
	}
	return srcAbs, dstAbs, existed, true
}

func davDone(w http.ResponseWriter, existed bool) {
	if existed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) davMove(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	src, dst, existed, ok := s.davTarget(w, r, u, root, rel)
	if !ok {
		return
	}
	if err := os.Rename(src, dst); err != nil {
		http.Error(w, "move failed", 500)
		return
	}
	s.invalidate(u.Name)
	davDone(w, existed)
}

func (s *Server) davCopy(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	srcAbs, ok1 := davPath(root, rel)
	if ok1 { // check the quota before touching anything
		var size int64
		if fi, err := os.Stat(srcAbs); err == nil {
			if fi.IsDir() {
				size = fsx.DirSize(srcAbs)
			} else {
				size = fi.Size()
			}
		}
		if s.usedBytes(u.Name, root, true)+size > u.QuotaBytes {
			http.Error(w, "Not enough space left in your NAS allowance", http.StatusInsufficientStorage)
			return
		}
	}
	src, dst, existed, ok := s.davTarget(w, r, u, root, rel)
	if !ok {
		return
	}
	if err := copyTree(src, dst); err != nil {
		http.Error(w, "copy failed", 500)
		return
	}
	s.invalidate(u.Name)
	davDone(w, existed)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// ---------- LOCK / UNLOCK (minimal, not enforced) ----------

func (s *Server) davLock(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	tok := "opaquelocktoken:" + auth.Token(8)
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.Header().Set("Lock-Token", "<"+tok+">")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock><D:locktype><D:write/></D:locktype><D:lockscope><D:exclusive/></D:lockscope><D:depth>0</D:depth><D:timeout>Second-3600</D:timeout><D:locktoken><D:href>%s</D:href></D:locktoken></D:activelock></D:lockdiscovery></D:prop>`, tok)
}

func (s *Server) davUnlock(w http.ResponseWriter, r *http.Request, u config.User, root, rel string) {
	w.WriteHeader(http.StatusNoContent)
}
