package server

import (
	"html"
	"net"
	"net/http"
	"strings"
)

// The device-setup listener serves ONLY public material (the NestNAS CA certificate, its Apple
// profile, and an instructions page) over plain HTTP. A new phone has to be able to fetch the
// certificate before it trusts anything, and iOS refuses to install profiles that were
// downloaded over a connection it does not yet trust. Nothing else is reachable here: no API,
// no files, no login. Because plain HTTP could be tampered with on a hostile network, the page
// shows the certificate fingerprint so it can be compared after installing.

// SetupHandler returns the handler for the device-setup port.
func (s *Server) SetupHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/NestNAS.mobileconfig", s.serveProfile)
	mux.HandleFunc("/ca.crt", s.serveCA)
	mux.HandleFunc("/ca.cer", s.serveCADER)
	mux.HandleFunc("/", s.setupPage)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// EnableSetupPort records the device-setup port so the dashboard can show its address.
func (s *Server) EnableSetupPort(p int) { s.SetupPort = p }

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || len(s.caPEM) == 0 {
		http.NotFound(w, r)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	https := "https://" + net.JoinHostPort(host, itoa(s.TLSPort)) + "/"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Cache-Control", "no-store")
	page := `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Set up this device for NestNAS</title>
<style>body{font:16px/1.5 system-ui,-apple-system,sans-serif;margin:0;background:#05080c;color:#e8f1f8}main{max-width:560px;margin:0 auto;padding:20px}
h1{font-size:22px}.card{background:#0a0f15;border:1px solid #1a2430;border-radius:14px;padding:16px;margin:14px 0}
a.b{display:block;text-align:center;background:#6dc8ff;color:#03121c;font-weight:700;text-decoration:none;padding:14px;border-radius:12px;margin:8px 0}
a.g{background:transparent;color:#6dc8ff;border:1px solid #1a2430;font-weight:600}
code{background:#05080c;padding:2px 6px;border-radius:6px;word-break:break-all}.m{color:#8da2b5;font-size:13px}ol{padding-left:20px}li{margin:6px 0}</style></head><body><main>
<svg viewBox="4 8 56 50" width="44" height="40" aria-hidden="true" style="display:block;margin:0 0 14px"><defs><linearGradient id="a" gradientUnits="userSpaceOnUse" x1="0" y1="30" x2="0" y2="53"><stop offset="0" stop-color="#5ec1ff"/><stop offset="1" stop-color="#1f6fff"/></linearGradient><linearGradient id="b" gradientUnits="userSpaceOnUse" x1="0" y1="30" x2="0" y2="46"><stop offset="0" stop-color="#8fdcff"/><stop offset="1" stop-color="#3b95ff"/></linearGradient><linearGradient id="c" gradientUnits="userSpaceOnUse" x1="0" y1="30" x2="0" y2="39"><stop offset="0" stop-color="#d9f4ff"/><stop offset="1" stop-color="#6dc8ff"/></linearGradient><linearGradient id="d" gradientUnits="userSpaceOnUse" x1="0" y1="14" x2="0" y2="30"><stop offset="0" stop-color="#fff"/><stop offset="1" stop-color="#9fdcff"/></linearGradient></defs><g fill="none" stroke-linecap="round"><path d="M10 30a22 22 0 0 0 44 0" stroke="url(#a)" stroke-width="4.4"/><path d="M17 30a15 15 0 0 0 30 0" stroke="url(#b)" stroke-width="4.4"/><path d="M24 30a8 8 0 0 0 16 0" stroke="url(#c)" stroke-width="4.4"/></g><path d="M32 14.5l7.5 7.5-7.5 7.5-7.5-7.5z" fill="url(#d)" stroke="url(#d)" stroke-width="2.2" stroke-linejoin="round"/></svg><h1>Set up this device for NestNAS</h1>
<p>Do this once per phone or computer so it can talk to your NAS over encrypted HTTPS.</p>
<div class="card"><b>iPhone / iPad</b>
<a class="b" href="/NestNAS.mobileconfig">Download the NestNAS profile</a>
<ol><li>Tap <b>Allow</b>.</li><li>Open <b>Settings</b> → <b>Profile Downloaded</b> → <b>Install</b> (it says "Not Signed"; that's normal).</li>
<li>Then <b>Settings → General → About → Certificate Trust Settings</b> and turn on <b>NestNAS Local CA</b>.</li></ol>
<div class="m">Use Safari for this, not another app.</div></div>
<div class="card"><b>Android, Windows, Mac, Linux</b>
<a class="b g" href="/ca.crt">Download certificate (.crt)</a><a class="b g" href="/ca.cer">Download certificate (binary .cer)</a>
<div class="m">Then install it as a trusted CA certificate in your system's settings. The NestNAS dashboard (Security tab) has the exact steps.</div></div>
<div class="card"><b>When that's done</b><p>Open the encrypted address: <a style="color:#6dc8ff" href="` + html.EscapeString(https) + `"><code>` + html.EscapeString(https) + `</code></a></p>
<div class="m">Network drive address (iPhone Files → Connect to Server): <code>` + html.EscapeString(https) + `dav/</code></div></div>
<div class="card m"><b>Optional safety check.</b> This page is plain HTTP, so on an untrusted network someone could tamper with it. After installing, the certificate's SHA-256 fingerprint should be exactly:<br><br><code>` + html.EscapeString(s.caPrint) + `</code><br><br>Only use this page on your own home or trusted network.</div>
</main></body></html>`
	_, _ = w.Write([]byte(page))
}
