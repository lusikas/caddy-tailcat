package caddytailcat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

func parse(t *testing.T, input string) (*Transport, error) {
	t.Helper()
	tr := new(Transport)
	return tr, tr.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input))
}

func TestUnmarshalCaddyfile(t *testing.T) {
	tr, err := parse(t, `tailcat tcABC {
		dial_timeout 2s
		derp_map_url https://example.com/derp.json
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Address != "tcABC" || time.Duration(tr.DialTimeout) != 2*time.Second || tr.DERPMapURL != "https://example.com/derp.json" {
		t.Errorf("got %+v", tr)
	}

	tr, err = parse(t, "tailcat tcABC {\n port 8080\n}")
	if err != nil || tr.Port != 8080 {
		t.Errorf("port: %+v, %v", tr, err)
	}

	tr, err = parse(t, "tailcat {\n address {env.X}\n}")
	if err != nil || tr.Address != "{env.X}" {
		t.Errorf("block address: %+v, %v", tr, err)
	}

	for _, bad := range []string{"tailcat a b", "tailcat {\n bogus 1\n}", "tailcat {\n dial_timeout nope\n}", "tailcat {\n port 0\n}", "tailcat {\n port 99999\n}", "tailcat {\n port http\n}"} {
		if _, err := parse(t, bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestPortFor(t *testing.T) {
	for _, c := range []struct {
		port uint16
		addr string
		want uint16
	}{
		{0, "127.0.0.1:8080", 8080}, // upstream's port
		{0, "localhost", 80},        // upstream without a port
		{0, "localhost:0", 80},
		{0, "[::1]:3000", 3000},
		{9000, "127.0.0.1:8080", 9000}, // configured port wins
		{9000, "localhost", 9000},
	} {
		if got := (&Transport{Port: c.port}).portFor(c.addr); got != c.want {
			t.Errorf("Port=%d upstream %q: got %d, want %d", c.port, c.addr, got, c.want)
		}
	}
}

func provision(t *testing.T, tr *Transport) error {
	t.Helper()
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	return tr.Provision(ctx)
}

func TestProvisionRejectsBadAddress(t *testing.T) {
	if err := provision(t, &Transport{}); err == nil {
		t.Error("empty address accepted")
	}
	err := provision(t, &Transport{Address: "not-a-tailcat-address"})
	if err == nil {
		t.Fatal("garbage address accepted")
	}
	if strings.Contains(err.Error(), "not-a-tailcat-address") {
		t.Errorf("error leaks the address: %v", err)
	}
}

// startServer starts a tailcat server whose port 8080 is a backend that
// answers "hello". It needs a DERP relay, so it is skipped with -short.
func startServer(t *testing.T) *tailcat.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a DERP relay")
	}
	// An explicit identity, so that a restarted server can be the same one;
	// Start doesn't write a generated key back into the Server.
	s := &tailcat.Server{
		Key:          key.NewNode(),
		PresharedKey: tailcat.NewPresharedKey(),
		Logf:         func(string, ...any) {},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ln, err := s.Listen(ctx, "tcp", ":8080")
	if err != nil {
		t.Skipf("can't start a tailcat server (offline?): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello ", r.URL.Path)
	}))
	return s
}

func get(t *testing.T, tr *Transport, url string) (string, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b), nil
}

func TestRoundTrip(t *testing.T) {
	s := startServer(t)
	tr := &Transport{Address: string(s.TailcatAddr()), DialTimeout: caddy.Duration(20 * time.Second)}
	if err := provision(t, tr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Cleanup() })

	// Caddy leaves the scheme empty and puts the dial address in Host; the
	// host is ignored and the port is the port on the tailcat server.
	got, err := get(t, tr, "//ignored.example:8080/x")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello /x" {
		t.Errorf("got %q", got)
	}

	// With Port set, the upstream's own port is irrelevant.
	tr.Port = 8080
	got, err = get(t, tr, "//localhost:1234/y")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello /y" {
		t.Errorf("got %q", got)
	}
}

// TestRecoversAfterServerRestart is the case that bit during manual testing:
// a tailcat client registers with its server once, so a server that restarts
// with the same identity ignores the old client.
func TestRecoversAfterServerRestart(t *testing.T) {
	s := startServer(t)
	addr := s.TailcatAddr()
	ci, err := tailcat.ParseAddr(addr)
	if err != nil {
		t.Fatal(err)
	}

	tr := &Transport{Address: string(addr), DialTimeout: caddy.Duration(8 * time.Second)}
	if err := provision(t, tr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Cleanup() })
	if _, err := get(t, tr, "//x:8080/"); err != nil {
		t.Fatal(err)
	}

	s.Close()
	tr.rt.CloseIdleConnections()
	if _, err := get(t, tr, "//x:8080/"); err == nil {
		t.Fatal("request succeeded with the server stopped")
	}

	// Same key, same relay region as the address embeds.
	s2 := &tailcat.Server{Key: s.Key, PresharedKey: s.PresharedKey, Region: ci.Region[0], Logf: func(string, ...any) {}}
	ln, err := s2.Listen(context.Background(), "tcp", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "back") }))

	got, err := get(t, tr, "//x:8080/")
	if err != nil {
		t.Fatalf("did not recover after the server restarted: %v", err)
	}
	if got != "back" {
		t.Errorf("got %q", got)
	}
}

func TestDialFailureIsAnErrorNotAHang(t *testing.T) {
	// An address nobody serves: the request must fail within DialTimeout.
	if testing.Short() {
		t.Skip("needs a DERP relay")
	}
	s := startServer(t)
	addr := s.TailcatAddr()
	s.Close()

	tr := &Transport{Address: string(addr), DialTimeout: caddy.Duration(2 * time.Second)}
	if err := provision(t, tr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Cleanup() })
	start := time.Now()
	_, err := get(t, tr, "//x:8080/")
	if err == nil {
		t.Fatal("expected an error")
	}
	var ne net.Error
	if !errors.As(err, &ne) && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("error type %T: %v", err, err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("took %v, want about the 2s dial timeout", d)
	}
}
