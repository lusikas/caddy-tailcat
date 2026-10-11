# caddy-tailcat

A Caddy `reverse_proxy` transport that reaches its upstream through a
[Tailcat](https://github.com/tailscale/tailcat) tunnel. A site is public through Caddy only while you
run `tailcat serve` for its address on your own machine. Stop it and the site
answers 504; start it again and the site is back. Nothing on your machine
listens on a public port.

## Use

1. On the machine with the thing to expose, make a permanent address once:

       tailcat genkey --key=blog --fixed-region     # prints tcXXXX...
       tailcat serve --key=blog 8080                # whenever you want it public

2. Give the address to Caddy (it is a secret, like a password):

       TAILCAT_ADDR=tcXXXX...

3. Caddyfile:

       blog.example.com {
           reverse_proxy localhost {
               transport tailcat {
                   address {env.TAILCAT_ADDR}
                   port 8080                  # port on the tailcat server
                   dial_timeout 4s            # how long to wait before the 504
               }
           }
       }

## Ports

The `reverse_proxy` upstream is only a placeholder: its host is ignored, since
the connection goes to the tailcat server, not to that host. The port to dial
on the tailcat server is the `port` option. Without it, the upstream's port is
used (`reverse_proxy 127.0.0.1:8080` means port 8080 on the tailcat server,
80 if the upstream has no port), which is the same number you give to
`tailcat serve`.

The port is a port *of the tailcat server*, not of your laptop. To serve a
different local port under another number, use a port mapping on the
`tailcat serve` side: `tailcat serve --key=blog 80:8080` serves tailcat port 80
by proxying to localhost:8080, so Caddy can use the default port.

## Build

    xcaddy build \
      --with github.com/caddy-dns/cloudflare \
      --with github.com/lusikas/caddy-tailcat

To build from a local checkout instead of GitHub, add `=/path/to/caddy-tailcat`
after the module name. Tailcat promises no API stability, so go.mod pins one
tailcat commit; bump it deliberately (see "Dependency pins").

## Notes

- Down upstream: each request waits `dial_timeout` and gets 504. To fail fast,
  add `health_uri /` to `reverse_proxy`; Caddy then answers 503 at once while
  the check fails.
- After the server restarts, one request fails and the next recovers (a tailcat
  client registers with the server only once, so the plugin builds a new one
  after a failed dial).
- Anyone who learns the address can reach the exposed port directly over
  tailcat, bypassing Caddy. Keep it in an env var or secret store, not in git.
- Not tested: WebSocket upgrades, HTTP/2 to the upstream, a relayed-only path
  for large transfers, or a reload of Caddy while requests are in flight.

## Tests

    go test -short ./...   # offline: Caddyfile parsing and address validation
    go test ./...          # also starts real tailcat servers through a DERP relay

## Dependency pins

Don't `go get -u` this module. `tailcat` builds against specific `gvisor.dev/gvisor`
and `tailscale.com` commits (see tailcat's go.mod); newer gvisor snapshots don't
compile as a Go module. If the build breaks after an update, pin those back to
the versions in tailcat's go.mod.
