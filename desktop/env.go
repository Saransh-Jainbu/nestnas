package main

import (
	"os"
	"strings"
)

// cleanSnapEnv undoes environment variables injected by a *different* snap app (typically
// VS Code's integrated terminal). They point GTK at that snap's incompatible libraries and
// GSettings schemas, which makes GTK abort at startup. A normal launch from the app menu
// is unaffected, so this only matters when started from such a terminal.
func cleanSnapEnv() {
	if os.Getenv("SNAP") == "" || os.Getenv("SNAP_NAME") == "nestnas" {
		return
	}
	for _, k := range []string{"GTK_PATH", "GTK_EXE_PREFIX", "GTK_IM_MODULE_FILE", "GIO_MODULE_DIR", "GSETTINGS_SCHEMA_DIR", "GTK_MODULES"} {
		os.Unsetenv(k)
	}
	// The snap prepends its own folders; restore the original value it saved.
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasSuffix(k, "_VSCODE_SNAP_ORIG") {
			os.Setenv(strings.TrimSuffix(k, "_VSCODE_SNAP_ORIG"), v)
		}
	}
}
