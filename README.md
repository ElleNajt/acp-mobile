# acp-mobile

> **WIP** — Functional and in daily use, but still under active development.
>
> **Security warning:** This exposes ACP agent sessions over HTTP/WebSocket. It has hardening measures (authkey cookies, CSRF via Sec-Fetch-Site, DNS rebinding protection, CSP nonces, rate-limited auth, localhost-only binding) and is designed for use over Tailscale, but it hasn't had a dedicated security review. Be cautious about exposing machines with sensitive data or credentials.

Mobile web UI for [ACP](https://github.com/anthropics/acp) sessions running through [acp-multiplex](https://github.com/ElleNajt/acp-multiplex).

Discovers all live acp-multiplex sockets on the machine, groups them by project, and lets you chat with any session from your phone.

## Features

- **Session discovery** — automatically finds all active acp-multiplex sockets, groups by project
- **Chat interface** — WebSocket bridge to any session with markdown rendering, streaming, tool call display
- **File browser** — browse and view files from session working directories
- **Auth** — random 256-bit authkey (generated on first run, stored in `~/.acp-mobile/authkey`)
- **Security hardening** — CSRF protection, DNS rebinding protection, CSP headers, XSS-safe markdown

## Setup

```bash
go build -o acp-mobile .
./acp-mobile [port]  # default 8090
```

The server binds to `127.0.0.1` only. On first run it generates an authkey and prints a URL with the key embedded — open that URL to authenticate.

## Requirements

- Go 1.21+
- One or more [acp-multiplex](https://github.com/ElleNajt/acp-multiplex) proxies running
