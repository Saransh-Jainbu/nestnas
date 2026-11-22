package service

import (
	"strings"
	"testing"
)

func TestSystemdUnit(t *testing.T) {
	u := systemdUnit(Opts{Exe: "/home/a b/bin/nestnas", Port: 8080, Home: "/home/a b/.nestnas"})
	for _, want := range []string{
		`ExecStart="/home/a b/bin/nestnas" serve --port 8080 --home "/home/a b/.nestnas"`,
		"Restart=on-failure", "WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q:\n%s", want, u)
		}
	}
}

func TestPlistEscapes(t *testing.T) {
	p := launchdPlist(Opts{Exe: "/Users/a&b/nestnas", Port: 9000, Home: "/h"}, "/log")
	if !strings.Contains(p, "/Users/a&amp;b/nestnas") || !strings.Contains(p, "<string>9000</string>") {
		t.Errorf("bad plist:\n%s", p)
	}
}

func TestWindowsCommand(t *testing.T) {
	c := windowsTaskCommand(Opts{Exe: `C:\Program Files\n\nestnas.exe`, Port: 8080, Home: `C:\Users\a\.nestnas`})
	if !strings.HasPrefix(c, `"C:\Program Files\n\nestnas.exe" "serve"`) {
		t.Errorf("bad command: %s", c)
	}
}
