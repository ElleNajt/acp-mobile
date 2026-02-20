package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	csrf "filippo.io/csrf/gorilla"
	"golang.org/x/net/websocket"
	"tailscale.com/tsnet"
)

// checkWebSocketOrigin validates the Origin header on WebSocket upgrades.
func checkWebSocketOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Host != r.Host {
		return false
	}
	return true
}

func main() {
	ts := &tsnet.Server{
		Hostname: "acp-mobile",
	}
	defer ts.Close()

	ln, err := ts.ListenTLS("tcp", ":443")
	if err != nil {
		log.Fatalf("tsnet ListenTLS: %v", err)
	}
	defer ln.Close()

	status, err := ts.Up(context.Background())
	if err != nil {
		log.Fatalf("tsnet Up: %v", err)
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
			if !checkWebSocketOrigin(r) {
				return fmt.Errorf("origin not allowed: %s", r.Header.Get("Origin"))
			}
			config.Origin, _ = websocket.Origin(config, r)
			return nil
		},
	})

	mux.HandleFunc("/api/sessions", handleSessions)
	mux.HandleFunc("/files/list", handleFileList)
	mux.HandleFunc("/files/read", handleFileRead)

	csrfMiddleware := csrf.Protect(nil)

	hostname := "acp-mobile"
	if len(status.CertDomains) > 0 {
		hostname = status.CertDomains[0]
	}
	fmt.Println()
	fmt.Printf("  acp-mobile: https://%s\n", hostname)
	fmt.Println()

	log.Fatal(http.Serve(ln, csrfMiddleware(mux)))
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
	pid  int
	path string
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
			socks = append(socks, socketEntry{pid, filepath.Join(dir, name)})
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
			socks = append(socks, socketEntry{pid, filepath.Join(tmpdir, name)})
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
	Pid       int    `json:"pid"`
	SessionID string `json:"sessionId,omitempty"`
	Title     string `json:"title,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Project   string `json:"project,omitempty"`
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
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg struct {
			Result json.RawMessage `json:"result"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		if msg.Result != nil {
			var res struct {
				AgentInfo *struct {
					Title string `json:"title"`
					Name  string `json:"name"`
				} `json:"agentInfo"`
				SessionID string `json:"sessionId"`
				Cwd       string `json:"cwd"`
			}
			if err := json.Unmarshal(msg.Result, &res); err == nil {
				if res.AgentInfo != nil {
					info.Title = res.AgentInfo.Title
					if info.Title == "" {
						info.Title = res.AgentInfo.Name
					}
				}
				if res.SessionID != "" {
					info.SessionID = res.SessionID
				}
				if res.Cwd != "" {
					info.Cwd = res.Cwd
					info.Project = filepath.Base(info.Cwd)
				}
			}
		}

		if msg.Method == "session/update" && msg.Params != nil {
			var params struct {
				Update struct {
					Kind  string `json:"sessionUpdate"`
					Title string `json:"title"`
				} `json:"update"`
			}
			if err := json.Unmarshal(msg.Params, &params); err == nil {
				if params.Update.Kind == "title_update" && params.Update.Title != "" {
					info.Title = params.Update.Title
				}
			}
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

func bridgeWebSocket(ws *websocket.Conn, sockPath string) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Printf("ws: connect to %s: %v", sockPath, err)
		ws.Close()
		return
	}
	defer conn.Close()

	var once sync.Once
	closeAll := func() {
		ws.Close()
		conn.Close()
	}

	// WebSocket -> Unix socket
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

	// Unix socket -> WebSocket (line-buffered)
	func() {
		defer once.Do(closeAll)
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := conn.Read(tmp)
			if err != nil {
				if err != io.EOF {
					log.Printf("ws sock read: %v", err)
				}
				return
			}
			buf = append(buf, tmp[:n]...)
			if len(buf) > 1024*1024 {
				log.Printf("ws: buffer exceeded 1MB, disconnecting")
				return
			}
			for {
				nlIdx := -1
				for i, b := range buf {
					if b == '\n' {
						nlIdx = i
						break
					}
				}
				if nlIdx < 0 {
					break
				}
				line := buf[:nlIdx]
				if len(line) > 0 {
					websocket.Message.Send(ws, string(line))
				}
				buf = buf[nlIdx+1:]
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
	dirPath := r.URL.Query().Get("path")
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

	showHidden := r.URL.Query().Get("show_hidden") == "true"
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
	filePath := r.URL.Query().Get("path")
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
