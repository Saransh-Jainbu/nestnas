// NestNAS: turn any laptop into a NAS. Open source, one binary, no dependencies.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/Saransh-Jainbu/nestnas/internal/service"
	"github.com/Saransh-Jainbu/nestnas/pkg/app"
)

var version = "0.1.0-dev"

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		serve(args)
	case "service":
		serviceCmd(args)
	case "version", "-v", "--version":
		fmt.Println("nestnas", version)
	default:
		fmt.Fprintf(os.Stderr, "usage: nestnas [serve [--port N] [--home DIR]] | service install|uninstall|status | version\n")
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", envInt("NESTNAS_PORT", 8080), "plain-HTTP port (this computer only)")
	tlsPort := fs.Int("tls-port", envInt("NESTNAS_TLS_PORT", 8443), "encrypted HTTPS port for other devices (-1 to disable)")
	setupPort := fs.Int("setup-port", envInt("NESTNAS_SETUP_PORT", 8081), "plain-HTTP port serving ONLY the public certificate for new devices (-1 to disable)")
	home := fs.String("home", app.DefaultHome(), "where NestNAS keeps its settings")
	addr := fs.String("listen", "0.0.0.0", "address the HTTPS port listens on (127.0.0.1 = this machine only)")
	_ = fs.Parse(args)

	in, err := app.Start(app.Options{Home: *home, Port: *port, TLSPort: *tlsPort, SetupPort: *setupPort, Listen: *addr, Version: version})
	if err != nil {
		fatal(err)
	}
	fmt.Println("\n  NestNAS is running!")
	if !in.Configured() {
		fmt.Println("  First time? Open the link below to finish setup.")
	}
	fmt.Printf("\n  On this laptop:                  http://localhost:%d\n", *port)
	if in.TLSPort > 0 {
		for _, u := range app.DeviceURLs(in.TLSPort) {
			fmt.Printf("  From other devices (encrypted):  %s\n", u)
		}
		fmt.Printf("\n  Encrypted with TLS 1.3. A new phone/computer must install the NestNAS certificate once.\n")
		if in.SetupPort > 0 {
			for _, u := range app.SetupURLs(in.SetupPort) {
				fmt.Printf("  Easiest: open this on the new device:  %s\n", u)
			}
		}
		fmt.Printf("  Certificate fingerprint:\n  %s\n", in.CAPrint)
	}
	fmt.Println("\n  (Ctrl+C to stop)")

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println("\nBye!")
		_ = in.Close()
	}()
	if err := in.Wait(); err != nil {
		fatal(err)
	}
}

// serviceCmd: nestnas service install|uninstall|status — start NestNAS automatically.
func serviceCmd(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: nestnas service install|uninstall|status"))
	}
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	port := fs.Int("port", envInt("NESTNAS_PORT", 8080), "plain-HTTP port (this computer only)")
	tlsPort := fs.Int("tls-port", envInt("NESTNAS_TLS_PORT", 8443), "encrypted HTTPS port for other devices")
	setupPort := fs.Int("setup-port", envInt("NESTNAS_SETUP_PORT", 8081), "device-setup port (public certificate only)")
	home := fs.String("home", app.DefaultHome(), "NestNAS settings folder")
	_ = fs.Parse(args[1:])

	switch args[0] {
	case "install":
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		if err != nil {
			fatal(err)
		}
		notes, err := service.Install(service.Opts{Exe: exe, Port: *port, TLSPort: *tlsPort, SetupPort: *setupPort, Home: *home})
		if err != nil {
			fatal(err)
		}
		fmt.Printf("\n  NestNAS will now start automatically and is running in the background.\n  Open: http://localhost:%d\n\n", *port)
		for _, n := range notes {
			fmt.Println("  •", n)
		}
	case "uninstall":
		if err := service.Uninstall(); err != nil {
			fatal(err)
		}
		fmt.Println("Autostart removed. Your files and settings were not touched.")
	case "status":
		fmt.Println(service.Status())
	default:
		fatal(fmt.Errorf("unknown service command %q", args[0]))
	}
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
