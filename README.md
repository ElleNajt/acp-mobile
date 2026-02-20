# acp-mobile

Mobile web UI for [ACP](https://github.com/anthropics/acp) sessions running through [acp-multiplex](https://github.com/ElleNajt/acp-multiplex).

Discovers all live acp-multiplex sockets on the machine, shows them grouped by project, and lets you chat with any session from your phone.

## Setup

```bash
go build -o acp-mobile .
./acp-mobile
```

The server joins your tailnet as `acp-mobile` using [tsnet](https://tailscale.com/kb/1244/tsnet) and listens on HTTPS with automatic TLS certificates. On first run it will print a Tailscale auth URL — visit it to authorize the node.

Once authorized, the UI is available at `https://acp-mobile.<tailnet>.ts.net/` from any device on your tailnet.

## Security

- **Tailscale network**: only tailnet members can reach the server
- **TLS**: tsnet provisions Let's Encrypt certificates automatically
- **CSRF**: [filippo.io/csrf](https://pkg.go.dev/filippo.io/csrf) blocks cross-origin requests using browser `Sec-Fetch-Site` headers
- **WebSocket origin check**: blocks cross-origin WebSocket upgrades

## Requirements

- Go 1.21+
- [Tailscale](https://tailscale.com) account
- One or more [acp-multiplex](https://github.com/ElleNajt/acp-multiplex) proxies running
