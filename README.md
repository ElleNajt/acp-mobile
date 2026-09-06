# acp-mobile

> **WIP** — Functional and in daily use, but still under active development.
>
> **Security warning:** This exposes ACP agent sessions over HTTP/WebSocket. It has hardening measures (authkey cookies, CSRF via Sec-Fetch-Site, DNS rebinding protection, CSP nonces, rate-limited auth, localhost-only binding) and is designed for use over Tailscale, but it hasn't had a dedicated security review. Be cautious about exposing machines with sensitive data or credentials.

Mobile web UI for [ACP](https://github.com/zed-industries/agent-client-protocol) sessions running through [acp-multiplex](https://github.com/ElleNajt/acp-multiplex).

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

The server always binds to `127.0.0.1`. If Tailscale is installed and connected, it also binds to the machine's Tailscale IP and prints a Tailscale URL; open that URL on the iPhone to authenticate. On first run it generates an authkey.

On macOS, build and run acp-mobile on the Mac that owns the ACP sockets, then open the printed Tailscale URL in Safari on the iPhone. For Zed, start an ACP-compatible session through Zed and leave its `acp-multiplex` proxy running; acp-mobile discovers that session automatically. The session chat and file browser work independently of Emacs. Spawn and Kill controls appear only when the corresponding agent-shell/Emacs helper is installed, so Zed-only setups will not show controls they cannot use.

## Session names

acp-mobile shows session names from acp-multiplex. To pass agent-shell buffer names through, set the `ACP_MULTIPLEX_NAME` environment variable when spawning the acp-multiplex process. In agent-shell, this requires injecting it into `:environment-variables` (not `process-environment`) because `acp.el` starts the process lazily:

```elisp
(advice-add 'agent-shell--make-acp-client :around
            (lambda (orig-fn &rest args)
              (let* ((buf (plist-get args :context-buffer))
                     (name-var (when buf
                                 (format "ACP_MULTIPLEX_NAME=%s" (buffer-name buf)))))
                (when name-var
                  (plist-put args :environment-variables
                             (cons name-var (plist-get args :environment-variables))))
                (apply orig-fn args))))
```

## Stopping acp-mobile

Don't use `pkill -f acp-mobile` — on macOS, `-f` can match unrelated processes (confirmed empirically: `pgrep -f acp-mobile` matches claude agent processes). Best guess is that node's `process.title` argv rewriting causes `pgrep -f` to search into environment variable data in `KERN_PROCARGS2`. Use `pkill -x acp-mobile` instead.

## Requirements

- Go 1.25.5 (the version declared in `go.mod`)
- One or more [acp-multiplex](https://github.com/ElleNajt/acp-multiplex) proxies running
