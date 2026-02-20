package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
)

func main() {
	port := "8090"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "index.html")
	})

	mux.Handle("/ws", &websocket.Server{
		Handler: func(ws *websocket.Conn) {
			pid := ws.Request().URL.Query().Get("sock")
			if pid == "" {
				log.Printf("ws: missing sock param")
				ws.Close()
				return
			}
			sockPath := findSocket(pid)
			if sockPath == "" {
				log.Printf("ws: no socket for pid %s", pid)
				ws.Close()
				return
			}
			bridgeWebSocket(ws, sockPath)
		},
		Handshake: func(config *websocket.Config, r *http.Request) error {
			config.Origin, _ = websocket.Origin(config, r)
			return nil
		},
	})

	mux.HandleFunc("/api/sessions", handleSessions)
	mux.HandleFunc("/files/list", handleFileList)
	mux.HandleFunc("/files/read", handleFileRead)

	addr := fmt.Sprintf("127.0.0.1:%s", port)
	log.Printf("acp-mobile: http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// --- Socket discovery ---

func socketDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "acp-multiplex")
}

type socketEntry struct {
	pid   int
	path  string
	mtime int64
}

func discoverSockets() []socketEntry {
	var socks []socketEntry
	seen := map[int]bool{}

	// New location: $TMPDIR/acp-multiplex/<pid>.sock
	dir := socketDir()
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".sock") {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSuffix(name, ".sock"))
			if err != nil {
				continue
			}
			if syscall.Kill(pid, 0) != nil {
				os.Remove(filepath.Join(dir, name))
				continue
			}
			seen[pid] = true
			var mt int64
			if info, err := e.Info(); err == nil {
				mt = info.ModTime().Unix()
			}
			socks = append(socks, socketEntry{pid, filepath.Join(dir, name), mt})
		}
	}

	// Legacy: $TMPDIR/acp-multiplex-<pid>.sock
	tmpdir := os.TempDir()
	if entries, err := os.ReadDir(tmpdir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "acp-multiplex-") || !strings.HasSuffix(name, ".sock") {
				continue
			}
			pidStr := strings.TrimSuffix(strings.TrimPrefix(name, "acp-multiplex-"), ".sock")
			pid, err := strconv.Atoi(pidStr)
			if err != nil || seen[pid] {
				continue
			}
			if syscall.Kill(pid, 0) != nil {
				os.Remove(filepath.Join(tmpdir, name))
				continue
			}
			var mt int64
			if info, err := e.Info(); err == nil {
				mt = info.ModTime().Unix()
			}
			socks = append(socks, socketEntry{pid, filepath.Join(tmpdir, name), mt})
		}
	}

	return socks
}

func findSocket(pidStr string) string {
	p := filepath.Join(socketDir(), pidStr+".sock")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	p = filepath.Join(os.TempDir(), "acp-multiplex-"+pidStr+".sock")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// --- Session probing ---

type sessionInfo struct {
	Pid          int    `json:"pid"`
	SessionID    string `json:"sessionId,omitempty"`
	Title        string `json:"title,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Project      string `json:"project,omitempty"`
	BufferName   string `json:"bufferName,omitempty"`
	LastActivity int64  `json:"lastActivity"` // unix timestamp
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	socks := discoverSockets()

	type result struct {
		info sessionInfo
		ok   bool
	}

	var wg sync.WaitGroup
	results := make([]result, len(socks))

	for i, s := range socks {
		idx := i
		sock := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			info := probeSocket(sock.path, sock.pid)
			info.LastActivity = sock.mtime
			results[idx] = result{info: info, ok: true}
		}()
	}
	wg.Wait()

	var sessions []sessionInfo
	for _, r := range results {
		if r.ok {
			sessions = append(sessions, r.info)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"sessions": sessions})
}

func probeSocket(sockPath string, pid int) sessionInfo {
	info := sessionInfo{Pid: pid}

	info.Cwd = processCwd(pid)
	if info.Cwd != "" {
		info.Project = filepath.Base(info.Cwd)
	}

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return info
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(1 * time.Second))

	// Read raw bytes instead of scanning lines — much faster for large replays.
	// We only need the first ~2 response messages (initialize + session/new).
	buf := make([]byte, 64*1024)
	var data []byte
	gotSessionID := false
	gotTitle := false

	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
			// Parse complete lines we have so far
			for {
				nl := bytes.IndexByte(data, '\n')
				if nl < 0 {
					break
				}
				line := data[:nl]
				data = data[nl+1:]

				if len(line) == 0 {
					continue
				}

				var msg struct {
					Result json.RawMessage `json:"result"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.Unmarshal(line, &msg) != nil {
					continue
				}

				// Check for acp-multiplex/meta notification
				if msg.Method == "acp-multiplex/meta" && msg.Params != nil {
					var meta struct {
						Name string `json:"name"`
					}
					if json.Unmarshal(msg.Params, &meta) == nil && meta.Name != "" {
						info.BufferName = meta.Name
					}
					continue
				}

				if msg.Result == nil {
					continue
				}

				var res struct {
					AgentInfo *struct {
						Title string `json:"title"`
						Name  string `json:"name"`
					} `json:"agentInfo"`
					SessionID string `json:"sessionId"`
					Cwd       string `json:"cwd"`
				}
				if json.Unmarshal(msg.Result, &res) != nil {
					continue
				}

				if res.AgentInfo != nil {
					info.Title = res.AgentInfo.Title
					if info.Title == "" {
						info.Title = res.AgentInfo.Name
					}
					gotTitle = true
				}
				if res.SessionID != "" {
					info.SessionID = res.SessionID
					gotSessionID = true
				}
				if res.Cwd != "" {
					info.Cwd = res.Cwd
					info.Project = filepath.Base(info.Cwd)
				}

				if gotSessionID && gotTitle {
					return info
				}
			}
		}
		if err != nil {
			break
		}
	}

	return info
}

func processCwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-d", "cwd", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return line[1:]
		}
	}
	return ""
}

// --- WebSocket bridge ---

// bridgeWebSocket connects to the proxy socket and bridges to the browser.
// Reads the replay, keeping only responses and the last N notifications,
// then forwards live traffic.
func bridgeWebSocket(ws *websocket.Conn, sockPath string) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Printf("ws: connect to %s: %v", sockPath, err)
		ws.Close()
		return
	}
	defer conn.Close()

	// Read the replay into memory using a short idle timeout to detect
	// when the replay burst is done (no explicit end marker from the proxy).
	var responses [][]byte
	var notifications [][]byte
	const tailSize = 40

	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 256*1024)

	for {
		// Short deadline: if no data arrives within 150ms, replay is done.
		conn.SetDeadline(time.Now().Add(150 * time.Millisecond))
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break
				}
				line := make([]byte, nl)
				copy(line, buf[:nl])
				buf = buf[nl+1:]
				if len(line) == 0 {
					continue
				}
				if bytes.Contains(line, []byte(`"result"`)) || bytes.Contains(line, []byte(`"error"`)) {
					responses = append(responses, line)
				} else {
					notifications = append(notifications, line)
				}
			}
		}
		if err != nil {
			break // timeout or EOF — replay is done
		}
	}

	conn.SetDeadline(time.Time{})

	// Send trimmed replay: all responses + last N notifications
	for _, line := range responses {
		websocket.Message.Send(ws, string(line))
	}
	tail := notifications
	if len(tail) > tailSize {
		tail = tail[len(tail)-tailSize:]
	}
	for _, line := range tail {
		websocket.Message.Send(ws, string(line))
	}

	log.Printf("ws: replay trimmed %d responses + %d/%d notifications",
		len(responses), len(tail), len(notifications))

	// Bridge live traffic
	var once sync.Once
	closeAll := func() {
		ws.Close()
		conn.Close()
	}

	go func() {
		defer once.Do(closeAll)
		for {
			var msg string
			if err := websocket.Message.Receive(ws, &msg); err != nil {
				if err != io.EOF {
					log.Printf("ws recv: %v", err)
				}
				return
			}
			conn.Write([]byte(msg))
			conn.Write([]byte("\n"))
		}
	}()

	func() {
		defer once.Do(closeAll)
		// Flush leftover bytes from replay read
		for {
			nl := bytes.IndexByte(buf, '\n')
			if nl < 0 {
				break
			}
			if nl > 0 {
				websocket.Message.Send(ws, string(buf[:nl]))
			}
			buf = buf[nl+1:]
		}
		for {
			n, err := conn.Read(tmp)
			if err != nil {
				if err != io.EOF {
					log.Printf("ws sock read: %v", err)
				}
				return
			}
			buf = append(buf, tmp[:n]...)
			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break
				}
				if nl > 0 {
					websocket.Message.Send(ws, string(buf[:nl]))
				}
				buf = buf[nl+1:]
			}
		}
	}()
}

// --- File browser ---

type fileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size,omitempty"`
}

func handleFileList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path       string `json:"path"`
		ShowHidden bool   `json:"showHidden"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	dirPath := req.Path
	if dirPath == "" {
		dirPath = "."
	}

	absPath, err := filepath.Abs(dirPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	showHidden := req.ShowHidden
	var files []fileEntry
	for _, e := range entries {
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fe := fileEntry{Name: name, IsDir: e.IsDir()}
		if !e.IsDir() {
			fe.Size = info.Size()
		}
		files = append(files, fe)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"path":  absPath,
		"files": files,
	})
}

func handleFileRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	filePath := req.Path
	if filePath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}
	if info.Size() > 1024*1024 {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"path":    absPath,
		"content": string(data),
	})
}
