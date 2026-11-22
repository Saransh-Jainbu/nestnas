// Package service installs NestNAS as a background service that starts automatically:
// systemd (user unit) on Linux, launchd (LaunchAgent) on macOS, Task Scheduler on Windows.
// Everything runs as the current user, so no root/admin rights are needed.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Opts struct {
	Exe       string // absolute path of the nestnas binary
	Port      int
	TLSPort   int    // 0 = leave at the default
	SetupPort int    // 0 = leave at the default
	Home      string // settings folder
}

func (o Opts) args() []string {
	a := []string{"serve", "--port", fmt.Sprint(o.Port)}
	if o.TLSPort != 0 {
		a = append(a, "--tls-port", fmt.Sprint(o.TLSPort))
	}
	if o.SetupPort != 0 {
		a = append(a, "--setup-port", fmt.Sprint(o.SetupPort))
	}
	return append(a, "--home", o.Home)
}

// ---------- rendering (pure, unit-tested) ----------

func systemdUnit(o Opts) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=NestNAS personal cloud\nAfter=network-online.target\nWants=network-online.target\n\n")
	b.WriteString("[Service]\nExecStart=" + quoteSystemd(o.Exe))
	for _, a := range o.args() {
		b.WriteString(" " + quoteSystemd(a))
	}
	b.WriteString("\nRestart=on-failure\nRestartSec=3\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func quoteSystemd(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func launchdPlist(o Opts, logPath string) string {
	var args strings.Builder
	for _, a := range append([]string{o.Exe}, o.args()...) {
		args.WriteString("    <string>" + xmlEsc(a) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>io.nestnas.server</string>
  <key>ProgramArguments</key><array>
` + args.String() + `  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + xmlEsc(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEsc(logPath) + `</string>
</dict></plist>
`
}

func windowsTaskCommand(o Opts) string {
	parts := []string{`"` + o.Exe + `"`}
	for _, a := range o.args() {
		parts = append(parts, `"`+a+`"`)
	}
	return strings.Join(parts, " ")
}

// ---------- install / uninstall / status ----------

func unitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", "nestnas.service")
}

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "io.nestnas.server.plist")
}

func sh(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Install registers the service and starts it now. Returns human-readable notes.
func Install(o Opts) (notes []string, err error) {
	switch runtime.GOOS {
	case "linux":
		if _, e := exec.LookPath("systemctl"); e != nil {
			return nil, fmt.Errorf("systemd not found; run `nestnas serve` manually or add it to your init system")
		}
		p := unitPath()
		if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return
		}
		if err = os.WriteFile(p, []byte(systemdUnit(o)), 0o644); err != nil {
			return
		}
		// "restart" (not just enable --now) so re-running install after an upgrade swaps in the new binary.
		for _, c := range [][]string{{"daemon-reload"}, {"enable", "nestnas.service"}, {"restart", "nestnas.service"}} {
			if err = sh("systemctl", append([]string{"--user"}, c...)...); err != nil {
				return
			}
		}
		// Linger lets the user service start at boot, before anyone logs in.
		if u := os.Getenv("USER"); u != "" {
			if sh("loginctl", "enable-linger", u) != nil {
				notes = append(notes, "To start at boot even before you log in, run once: sudo loginctl enable-linger "+u)
			}
		}
		notes = append(notes, "Laptop tip: closing the lid suspends it and takes your NAS offline. To keep it running, set HandleLidSwitch=ignore in /etc/systemd/logind.conf (then: sudo systemctl restart systemd-logind).")
	case "darwin":
		p := plistPath()
		home, _ := os.UserHomeDir()
		if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return
		}
		if err = os.WriteFile(p, []byte(launchdPlist(o, filepath.Join(home, "Library", "Logs", "nestnas.log"))), 0o644); err != nil {
			return
		}
		_ = sh("launchctl", "unload", p)
		if err = sh("launchctl", "load", "-w", p); err != nil {
			return
		}
		notes = append(notes, "Laptop tip: to stay reachable with the lid closed, keep it plugged in and run: sudo pmset -c sleep 0 (or use System Settings > Battery > Options).")
	case "windows":
		if err = sh("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", "NestNAS", "/TR", windowsTaskCommand(o)); err != nil {
			return
		}
		_ = sh("schtasks", "/End", "/TN", "NestNAS") // stop an older running copy, if any
		_ = sh("schtasks", "/Run", "/TN", "NestNAS")
		notes = append(notes, "NestNAS starts when you log in to Windows. Laptop tip: Settings > System > Power > set 'when plugged in, put my device to sleep' to Never.")
	default:
		return nil, fmt.Errorf("autostart isn't supported on %s yet", runtime.GOOS)
	}
	return
}

func Uninstall() error {
	switch runtime.GOOS {
	case "linux":
		_ = sh("systemctl", "--user", "disable", "--now", "nestnas.service")
		_ = os.Remove(unitPath())
		return sh("systemctl", "--user", "daemon-reload")
	case "darwin":
		_ = sh("launchctl", "unload", "-w", plistPath())
		return os.Remove(plistPath())
	case "windows":
		_ = sh("schtasks", "/End", "/TN", "NestNAS")
		return sh("schtasks", "/Delete", "/F", "/TN", "NestNAS")
	}
	return fmt.Errorf("unsupported OS")
}

// Status returns a one-line description of whether the service is running.
func Status() string {
	switch runtime.GOOS {
	case "linux":
		out, _ := exec.Command("systemctl", "--user", "is-active", "nestnas.service").Output()
		return "systemd user service: " + strings.TrimSpace(string(out))
	case "darwin":
		if err := exec.Command("launchctl", "list", "io.nestnas.server").Run(); err == nil {
			return "launchd agent: loaded"
		}
		return "launchd agent: not installed"
	case "windows":
		out, _ := exec.Command("schtasks", "/Query", "/TN", "NestNAS").CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	return "unsupported OS"
}
