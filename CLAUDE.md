# acp-mobile

Mobile web frontend for ACP (Agent Communication Protocol) sessions.

## Debugging

Use `ssh fugue` for testing. Start acp-mobile in test mode on a separate port:

```bash
ssh fugue 'cd ~/code/acp-mobile && ./acp-mobile --test-mode 18091 &'
```

`--test-mode` skips Origin header checks on WebSocket, so you can connect with curl/python/websocat.

### Fake replay server

To test replay scenarios, use a fake socket server that sends crafted messages:

1. Write a Go program that listens on a Unix socket in `$XDG_RUNTIME_DIR/acp-multiplex/<pid>.sock`
2. Send JSON-RPC messages matching the ACP protocol
3. acp-mobile discovers sockets by PID, so the socket filename must match the server's PID
4. Connect via WebSocket: `ws://127.0.0.1:18091/ws?sock=<pid>`

### Connecting to replay via WebSocket

```python
import websocket, json
ws = websocket.create_connection("ws://127.0.0.1:18091/ws?sock=<pid>", timeout=3)
while True:
    msg = json.loads(ws.recv())
    print(json.dumps(msg, indent=2)[:200])
```

### Deploying changes to fugue

```bash
scp index.html fugue:~/code/acp-mobile/index.html
ssh fugue 'cd ~/code/acp-mobile && CGO_ENABLED=0 go build -o acp-mobile .'
# Restart: pkill and relaunch
```
