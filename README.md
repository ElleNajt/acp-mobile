# acp-mobile

Mobile web UI for [ACP](https://github.com/anthropics/acp) sessions running through [acp-multiplex](https://github.com/ElleNajt/acp-multiplex).

Discovers all live acp-multiplex sockets on the machine, shows them grouped by project, and lets you chat with any session from your phone.

## Setup

```bash
go build -o acp-mobile .
./acp-mobile [port]  # default 8090
```

The server binds to `127.0.0.1` only. To access it from your phone, register it with [agent-to-go](https://github.com/ElleNajt/agent-to-go):

```bash
agent-to-go --web-app acp-mobile=8090
```

This makes the UI available at `https://<tailnet-host>/app/acp-mobile/` from any device on your tailnet. agent-to-go handles Tailscale networking, TLS, and CSRF protection.

## Requirements

- Go 1.21+
- One or more [acp-multiplex](https://github.com/ElleNajt/acp-multiplex) proxies running
