// Package caddytailcat is a Caddy reverse_proxy transport that dials its
// upstream through a Tailcat tunnel instead of the network.
//
//	example.com {
//		reverse_proxy localhost {
//			transport tailcat {
//				address tcXXXXXXXX
//				port 8080
//			}
//		}
//	}
//
// Caddy proxies example.com to port 8080 of the tailcat server at that
// address, as served by "tailcat serve 8080". The reverse_proxy upstream is
// only a placeholder; its host is ignored. While that command isn't running
// there is nothing to dial, and Caddy answers 502 (or 503 with health checks).
package caddytailcat

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/tailscale/tailcat"
)

func init() {
	caddy.RegisterModule((*Transport)(nil))
}

// Transport is an http.RoundTripper whose connections go through a tailcat
// tunnel to the server at Address. The upstream's host is ignored. The port
// on the tailcat server is Port, or if that is unset, the upstream's port.
type Transport struct {
	// Address is the tailcat address of the server, as printed by
	// "tailcat serve". It is a secret: treat it like a password. It may
	// contain placeholders such as {env.TAILCAT_ADDR}.
	Address string `json:"address,omitempty"`

	// Port is the port on the tailcat server to connect to. If zero, it is
	// the port of the reverse_proxy upstream (80 if that has none), which
	// is easy to confuse with a local port: the upstream's host is ignored.
	Port uint16 `json:"port,omitempty"`

	// DialTimeout bounds setting up the tunnel and opening a connection.
	// Default 5s. It is how long a request waits before getting a 502 when
	// nobody is serving the address.
	DialTimeout caddy.Duration `json:"dial_timeout,omitempty"`

	// DERPMapURL, if set, is where to fetch the DERP map from when the
	// address doesn't embed its relay. Default is tailcat.dev's.
	DERPMapURL string `json:"derp_map_url,omitempty"`

	addr   tailcat.Addr
	logf   func(format string, args ...any)
	rt     *http.Transport
	mu     sync.Mutex
	client *tailcat.Client // nil until first use, and after a failed dial
}

// CaddyModule implements caddy.Module.
func (*Transport) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.transport.tailcat",
		New: func() caddy.Module { return new(Transport) },
	}
}

// Provision implements caddy.Provisioner.
func (t *Transport) Provision(ctx caddy.Context) error {
	addr := caddy.NewReplacer().ReplaceAll(t.Address, "")
	if addr == "" {
		return errors.New("tailcat transport: address is required")
	}
	if _, err := tailcat.ParseAddr(tailcat.Addr(addr)); err != nil {
		// Don't echo the address: it is a secret.
		return fmt.Errorf("tailcat transport: invalid address: %w", err)
	}
	if t.DialTimeout == 0 {
		t.DialTimeout = caddy.Duration(5 * time.Second)
	}

	logger := ctx.Logger()
	t.addr = tailcat.Addr(addr)
	t.logf = func(format string, args ...any) {
		logger.Debug(fmt.Sprintf(format, args...))
	}

	t.rt = &http.Transport{
		DialContext:           t.dial,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // Caddy's reverse_proxy has its own timeouts
	}
	return nil
}

// portFor returns the port on the tailcat server to dial for a reverse_proxy
// upstream address: the configured Port, else the upstream's own port.
func (t *Transport) portFor(address string) uint16 {
	if t.Port != 0 {
		return t.Port
	}
	if _, p, err := net.SplitHostPort(address); err == nil {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil && n != 0 {
			return uint16(n)
		}
	}
	return 80
}

func (t *Transport) dial(ctx context.Context, _, address string) (net.Conn, error) {
	port := t.portFor(address)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.DialTimeout))
	defer cancel()

	c := t.getClient()
	conn, err := c.DialTCPPort(ctx, port)
	if err != nil {
		// A client registers with the server once, so if the server was
		// restarted (or was never up) this one is useless. Start over with a
		// fresh client on the next request.
		t.drop(c)
		return nil, err
	}
	return conn, nil
}

func (t *Transport) getClient() *tailcat.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == nil {
		t.client = tailcat.NewClient(t.addr)
		t.client.DERPMapURL = t.DERPMapURL
		t.client.Logf = t.logf
	}
	return t.client
}

func (t *Transport) drop(c *tailcat.Client) {
	t.mu.Lock()
	if t.client == c {
		t.client = nil
	}
	t.mu.Unlock()
	t.rt.CloseIdleConnections()
	c.Close()
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// reverse_proxy leaves the scheme to the transport. The tunnel is already
	// encrypted end to end, so plain HTTP inside it is right.
	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}
	return t.rt.RoundTrip(req)
}

// Cleanup implements caddy.CleanerUpper.
func (t *Transport) Cleanup() error {
	if t.rt != nil {
		t.rt.CloseIdleConnections()
	}
	t.mu.Lock()
	c := t.client
	t.client = nil
	t.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler:
//
//	tailcat [<address>] {
//		address      <address>
//		port         <port on the tailcat server>
//		dial_timeout <duration>
//		derp_map_url <url>
//	}
func (t *Transport) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the module name
	if d.NextArg() {
		t.Address = d.Val()
	}
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "address":
			if !d.AllArgs(&t.Address) {
				return d.ArgErr()
			}
		case "port":
			var s string
			if !d.AllArgs(&s) {
				return d.ArgErr()
			}
			n, err := strconv.ParseUint(s, 10, 16)
			if err != nil || n == 0 {
				return d.Errf("bad port %q", s)
			}
			t.Port = uint16(n)
		case "dial_timeout":
			var s string
			if !d.AllArgs(&s) {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(s)
			if err != nil {
				return d.Errf("bad dial_timeout: %v", err)
			}
			t.DialTimeout = caddy.Duration(dur)
		case "derp_map_url":
			if !d.AllArgs(&t.DERPMapURL) {
				return d.ArgErr()
			}
		default:
			return d.Errf("unrecognized subdirective %q", d.Val())
		}
	}
	return nil
}

var (
	_ caddy.Module          = (*Transport)(nil)
	_ caddy.Provisioner     = (*Transport)(nil)
	_ caddy.CleanerUpper    = (*Transport)(nil)
	_ caddyfile.Unmarshaler = (*Transport)(nil)
	_ http.RoundTripper     = (*Transport)(nil)
)
