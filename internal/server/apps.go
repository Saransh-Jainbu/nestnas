package server

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

// ---------- remote access (Tailscale) ----------

type remoteInfo struct {
	Installed bool   `json:"installed"`
	Connected bool   `json:"connected"`
	IP        string `json:"ip"`
	Hint      string `json:"hint"`
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func remoteStatus() remoteInfo {
	ri := remoteInfo{Hint: "curl -fsSL https://tailscale.com/install.sh | sh && sudo tailscale up"}
	if _, err := exec.LookPath("tailscale"); err != nil {
		return ri
	}
	ri.Installed = true
	if ip, err := run(3*time.Second, "tailscale", "ip", "-4"); err == nil && ip != "" {
		ri.Connected, ri.IP = true, strings.Fields(ip)[0]
	} else {
		ri.Hint = "sudo tailscale up"
	}
	return ri
}

// ---------- app hosting (Docker) ----------

type appDef struct {
	ID      string
	Name    string
	Desc    string
	Image   string
	Ports   [][2]string // host, container
	Volumes [][2]string // subdir-or-special, container path
	Env     []string
}

var catalog = []appDef{
	{ID: "jellyfin", Name: "Jellyfin", Desc: "Stream your movies, shows and music to any device.",
		Image: "jellyfin/jellyfin", Ports: [][2]string{{"8096", "8096"}},
		Volumes: [][2]string{{"config", "/config"}, {"cache", "/cache"}, {"@media", "/media:ro"}}},
	{ID: "vaultwarden", Name: "Vaultwarden", Desc: "Your own password manager (Bitwarden compatible). Use HTTPS via Tailscale for browsers.",
		Image: "vaultwarden/server:latest", Ports: [][2]string{{"8222", "80"}}, Volumes: [][2]string{{"data", "/data"}}},
	{ID: "syncthing", Name: "Syncthing", Desc: "Continuously sync folders between your devices.",
		Image: "syncthing/syncthing", Ports: [][2]string{{"8384", "8384"}, {"22000", "22000"}},
		Volumes: [][2]string{{"data", "/var/syncthing"}}},
	{ID: "uptime-kuma", Name: "Uptime Kuma", Desc: "Monitor your websites and services with a pretty dashboard.",
		Image: "louislam/uptime-kuma:1", Ports: [][2]string{{"3001", "3001"}}, Volumes: [][2]string{{"data", "/app/data"}}},
}

func findApp(id string) (appDef, bool) {
	for _, a := range catalog {
		if a.ID == id {
			return a, true
		}
	}
	return appDef{}, false
}

func dockerStatus() map[string]interface{} {
	if _, err := exec.LookPath("docker"); err != nil {
		return map[string]interface{}{"installed": false, "hint": "curl -fsSL https://get.docker.com | sh"}
	}
	v, err := run(4*time.Second, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return map[string]interface{}{"installed": true, "running": false,
			"hint": "Docker is installed but not reachable. Start it, and add your user to the docker group."}
	}
	return map[string]interface{}{"installed": true, "running": true, "version": v}
}

func containerStates() map[string]string {
	out, err := run(5*time.Second, "docker", "ps", "-a", "--filter", "label=nestnas.app", "--format", `{{.Label "nestnas.app"}}={{.State}}`)
	states := map[string]string{}
	if err != nil {
		return states
	}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			states[k] = v
		}
	}
	return states
}

func (s *Server) apps(w http.ResponseWriter, r *http.Request, _ config.User) {
	ds := dockerStatus()
	states := map[string]string{}
	if ds["running"] == true {
		states = containerStates()
	}
	type view struct {
		ID, Name, Desc string
		Ports          []string
		State          string // "", running, exited...
	}
	var out []view
	for _, a := range catalog {
		v := view{ID: a.ID, Name: a.Name, Desc: a.Desc, State: states[a.ID]}
		for _, p := range a.Ports {
			v.Ports = append(v.Ports, p[0])
		}
		out = append(out, v)
	}
	jsonOut(w, 200, map[string]interface{}{"docker": ds, "apps": out})
}

func (s *Server) appInstall(w http.ResponseWriter, r *http.Request, me config.User) {
	var d struct{ ID string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	a, ok := findApp(d.ID)
	if !ok {
		jerr(w, 404, "unknown app")
		return
	}
	if dockerStatus()["running"] != true {
		jerr(w, 400, "Docker isn't running on this machine")
		return
	}
	if s.encrypted() {
		for _, v := range a.Volumes {
			if v[0] == "@media" {
				jerr(w, 400, a.Name+" needs to read your media files directly, which isn't possible while storage encryption is on.")
				return
			}
		}
	}
	base := filepath.Join(s.st.StoragePath(), "apps", a.ID)
	args := []string{"run", "-d", "--name", "nestnas-" + a.ID, "--restart", "unless-stopped", "--label", "nestnas.app=" + a.ID}
	for _, p := range a.Ports {
		args = append(args, "-p", p[0]+":"+p[1])
	}
	for _, v := range a.Volumes {
		src := filepath.Join(base, v[0])
		if v[0] == "@media" {
			src = filepath.Join(s.st.StoragePath(), "users", me.Name, "Media")
		}
		if err := os.MkdirAll(src, 0o755); err != nil {
			jerr(w, 500, "can't create data folder")
			return
		}
		args = append(args, "-v", src+":"+v[1])
	}
	args = append(args, a.Image)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute) // image pulls can be slow
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		jerr(w, 500, "Docker error: "+msg)
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) appRemove(w http.ResponseWriter, r *http.Request, _ config.User) {
	var d struct{ ID string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	a, ok := findApp(d.ID)
	if !ok {
		jerr(w, 404, "unknown app")
		return
	}
	// Data is kept on disk; only the container is removed.
	if out, err := run(60*time.Second, "docker", "rm", "-f", "nestnas-"+a.ID); err != nil {
		jerr(w, 500, "Docker error: "+out)
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}
