// Package app is NestNAS's public entry point for embedding the server in another program
// (for example the desktop app in ./desktop). It uses only the standard library.
package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/server"
	"github.com/Saransh-Jainbu/nestnas/internal/tlsutil"
)

type Options struct {
	Home      string // settings folder; "" = default (~/.nestnas or $NESTNAS_HOME)
	Port      int    // plain-HTTP port, reachable ONLY from this computer (127.0.0.1)
	TLSPort   int    // encrypted HTTPS port for other devices; 0 = default 8443, -1 = disabled
	SetupPort int    // plain-HTTP port that serves ONLY the public certificate for new devices; 0 = default 8081, -1 = disabled
	Listen    string // address the HTTPS port binds to; "" = all interfaces, so phones can connect
	Version   string
}

// Instance is a running NestNAS server.
type Instance struct {
	Port      int
	TLSPort   int // 0 if HTTPS is disabled
	SetupPort int // 0 if the device-setup page is disabled
	CAPrint   string
	store     *config.Store
	srvs      []*http.Server
	done      chan error
}

// DefaultHome returns where NestNAS keeps its settings.
func DefaultHome() string {
	if h := os.Getenv("NESTNAS_HOME"); h != "" {
		return h
	}
	u, _ := os.UserHomeDir()
	return filepath.Join(u, ".nestnas")
}

// localOnly rejects requests whose Host header isn't this computer. The loopback listener
// carries unencrypted traffic, so this also blocks DNS-rebinding attacks from web pages.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if i := strings.LastIndex(host, ":"); i > 0 && !strings.HasSuffix(host, "]") {
			host = host[:i]
		}
		if host != "localhost" && host != "127.0.0.1" && host != "[::1]" {
			http.Error(w, "This address only works on the computer running NestNAS. Use the https:// address from another device.", http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Start opens the settings, binds the ports and serves in the background. Plain HTTP is only
// ever offered on 127.0.0.1 (traffic that never leaves this computer); every other device must
// use HTTPS (TLS 1.3). It returns an error immediately if a port is already taken.
func Start(o Options) (*Instance, error) {
	if o.Home == "" {
		o.Home = DefaultHome()
	}
	if o.Port == 0 {
		o.Port = 8080
	}
	if o.TLSPort == 0 {
		o.TLSPort = 8443
	}
	if o.SetupPort == 0 {
		o.SetupPort = 8081
	}
	if o.Listen == "" {
		o.Listen = "0.0.0.0"
	}
	st, err := config.Open(o.Home)
	if err != nil {
		return nil, err
	}
	s := server.New(st, o.Version, o.Port)
	s.SetHome(o.Home)

	httpLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(o.Port)))
	if err != nil {
		return nil, err
	}
	in := &Instance{Port: o.Port, store: st, done: make(chan error, 2)}
	serve := func(srv *http.Server, ln net.Listener) {
		in.srvs = append(in.srvs, srv)
		go func() {
			err := srv.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			in.done <- err
		}()
	}
	mk := func(h http.Handler) *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	}

	if o.TLSPort > 0 {
		mgr, err := tlsutil.Open(filepath.Join(o.Home, "tls"))
		if err != nil {
			httpLn.Close()
			return nil, err
		}
		tlsLn, err := net.Listen("tcp", net.JoinHostPort(o.Listen, fmt.Sprint(o.TLSPort)))
		if err != nil {
			httpLn.Close()
			return nil, err
		}
		s.EnableTLS(o.TLSPort, mgr.CAPEM(), mgr.Fingerprint())
		in.TLSPort, in.CAPrint = o.TLSPort, mgr.Fingerprint()
		if o.SetupPort > 0 {
			setupLn, err := net.Listen("tcp", net.JoinHostPort(o.Listen, fmt.Sprint(o.SetupPort)))
			if err != nil {
				httpLn.Close()
				tlsLn.Close()
				return nil, err
			}
			s.EnableSetupPort(o.SetupPort)
			in.SetupPort = o.SetupPort
			serve(mk(s.SetupHandler()), setupLn)
		}
		serve(mk(s.Handler()), tls.NewListener(tlsLn, mgr.TLSConfig()))
	}
	serve(mk(localOnly(s.Handler())), httpLn)
	return in, nil
}

// Configured reports whether first-run setup has been completed.
func (i *Instance) Configured() bool { return i.store.Configured() }

// Wait blocks until the server stops.
func (i *Instance) Wait() error { return <-i.done }

// Close stops the server.
func (i *Instance) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var first error
	for _, s := range i.srvs {
		if err := s.Shutdown(ctx); err != nil {
			_ = s.Close()
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// LANURLs lists the plain-HTTP addresses (kept for compatibility).
func LANURLs(port int) []string { return server.LANURLs(port) }

// SetupURLs lists the plain-HTTP "set up a new device" addresses (public certificate only).
func SetupURLs(port int) []string { return server.LANURLsScheme("http", port) }

// DeviceURLs lists the encrypted addresses other devices on the network should use.
func DeviceURLs(tlsPort int) []string { return server.LANURLsScheme("https", tlsPort) }

// Probe reports whether a NestNAS server already answers on 127.0.0.1:port,
// so a second launch can attach to it instead of failing on a busy port.
func Probe(port int) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return false
	}
	_, ok := v["setup_needed"]
	return ok
}
