package main

import (
    "encoding/json"
    "flag"
    "fmt"
    "log"
    "net"
    "net/http"
    "os"
    "strings"
    "sync"
    "time"

    "github.com/gorilla/websocket"
)

// Build-time variables (set via -ldflags)
var (
    CommitHash = "dev"
    BuildTime  = "dev"
)

// Config via env/flags
type Config struct {
    HTTPPort      string
    UDPPort       string
    AllowedOrigin string
}

// Hub manages rooms and broadcasting
type Hub struct {
    mu    sync.RWMutex
    rooms map[string]*Room
}

type Room struct {
    name    string
    mu      sync.RWMutex
    clients map[*Client]bool
}

type Client struct {
    username string
    room     *Room
    conn     *websocket.Conn
    sendCh   chan []byte
}

func NewHub() *Hub {
    return &Hub{rooms: make(map[string]*Room)}
}

func (h *Hub) getRoom(name string) *Room {
    h.mu.Lock()
    defer h.mu.Unlock()
    r, ok := h.rooms[name]
    if !ok {
        r = &Room{name: name, clients: make(map[*Client]bool)}
        h.rooms[name] = r
    }
    return r
}

func (r *Room) join(c *Client) {
    r.mu.Lock()
    r.clients[c] = true
    r.mu.Unlock()
}

func (r *Room) leave(c *Client) {
    r.mu.Lock()
    delete(r.clients, c)
    r.mu.Unlock()
}

func (r *Room) broadcast(sender *Client, msg []byte) {
    r.mu.RLock()
    for c := range r.clients {
        if c != sender { // echo suppression; comment to echo self
            select {
            case c.sendCh <- msg:
            default:
                // drop if slow
            }
        }
    }
    r.mu.RUnlock()
}

var upgrader = websocket.Upgrader{
    ReadBufferSize:  8192,
    WriteBufferSize: 8192,
    CheckOrigin: func(r *http.Request) bool {
        // CORS is handled via headers; allow upgrade but enforce via ALLOWED_ORIGIN if needed
        return true
    },
}

// HandleWebSocket handles /ws/{room}/{username}
func HandleWebSocket(hub *Hub, allowedOrigin string) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        applyCORSHeaders(w, allowedOrigin)
        if r.Method == http.MethodOptions {
            w.WriteHeader(http.StatusNoContent)
            return
        }

        // Parse path: /ws/{room}/{username}
        // If missing, defaults: room="global", username="anon-<ts>"
        path := strings.TrimPrefix(r.URL.Path, "/ws")
        parts := splitTrim(path, '/')
        roomName := "global"
        username := fmt.Sprintf("anon-%d", time.Now().UnixNano())
        if len(parts) >= 1 && parts[0] != "" {
            roomName = parts[0]
        }
        if len(parts) >= 2 && parts[1] != "" {
            username = parts[1]
        }

        conn, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            log.Printf("websocket upgrade error: %v", err)
            return
        }

        room := hub.getRoom(roomName)
        client := &Client{
            username: username,
            room:     room,
            conn:     conn,
            sendCh:   make(chan []byte, 256),
        }
        room.join(client)
        log.Printf("client joined: room=%s user=%s", roomName, username)

        // Start writer
        go func() {
            defer func() {
                client.conn.Close()
            }()
            for msg := range client.sendCh {
                client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
                if err := client.conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
                    return
                }
            }
        }()

        // Reader loop
        for {
            client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
            msgType, msg, err := client.conn.ReadMessage()
            if err != nil {
                break
            }
            _ = msgType // treat both text/binary same; broadcast raw
            // Optional: wrap with minimal header
            envelope := MarshalEnvelope(roomName, client.username, msg)
            room.broadcast(client, envelope)
        }

        // cleanup
        room.leave(client)
        close(client.sendCh)
        log.Printf("client left: room=%s user=%s", roomName, username)
    }
}

type Envelope struct {
    Room     string `json:"room"`
    Username string `json:"username"`
    Ts       int64  `json:"ts"`
    Payload  []byte `json:"payload"`
}

func MarshalEnvelope(room, user string, payload []byte) []byte {
    env := Envelope{Room: room, Username: user, Ts: time.Now().UnixNano(), Payload: payload}
    b, _ := json.Marshal(env)
    return b
}

// UDP Relay: experimental, minimal broadcast of raw datagrams per-room.
// Protocol: first line "ROOM:<name>;USER:<username>\n" followed by binary.
func StartUDPRelay(udpPort string, hub *Hub) (*net.UDPConn, error) {
    addr, err := net.ResolveUDPAddr("udp", ":"+udpPort)
    if err != nil {
        return nil, err
    }
    conn, err := net.ListenUDP("udp", addr)
    if err != nil {
        return nil, err
    }

    type peer struct {
        addr *net.UDPAddr
        last time.Time
    }
    // simple peer registry per room
    var (
        mu    sync.Mutex
        rooms = map[string]map[string]*peer{} // room -> username -> peer
    )

    go func() {
        defer conn.Close()
        buf := make([]byte, 64*1024)
        for {
            n, remote, err := conn.ReadFromUDP(buf)
            if err != nil {
                log.Printf("udp read error: %v", err)
                return
            }
            data := buf[:n]
            roomName, username, payload := parseUDPFrame(data)
            if roomName == "" {
                roomName = "global"
            }
            if username == "" {
                username = fmt.Sprintf("udp-%d", time.Now().UnixNano())
            }
            mu.Lock()
            if _, ok := rooms[roomName]; !ok {
                rooms[roomName] = map[string]*peer{}
            }
            rooms[roomName][username] = &peer{addr: remote, last: time.Now()}
            // broadcast to all peers in room except sender
            for uname, p := range rooms[roomName] {
                if uname == username {
                    continue
                }
                _, _ = conn.WriteToUDP(payload, p.addr)
            }
            mu.Unlock()

            // also broadcast into websocket room
            env := MarshalEnvelope(roomName, username, payload)
            hub.getRoom(roomName).broadcast(nil, env)
        }
    }()
    return conn, nil
}

func parseUDPFrame(b []byte) (room, user string, payload []byte) {
    s := string(b)
    if i := strings.Index(s, "\n"); i >= 0 {
        header := s[:i]
        payload = []byte(s[i+1:])
        for _, part := range strings.Split(header, ";") {
            kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
            if len(kv) != 2 {
                continue
            }
            k := strings.ToUpper(strings.TrimSpace(kv[0]))
            v := strings.TrimSpace(kv[1])
            switch k {
            case "ROOM":
                room = v
            case "USER":
                user = v
            }
        }
        return
    }
    return "", "", b
}

func applyCORSHeaders(w http.ResponseWriter, allowedOrigin string) {
    if allowedOrigin == "" {
        allowedOrigin = "*"
    }
    w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
    w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
    w.Header().Set("Access-Control-Max-Age", "3600")
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
    applyCORSHeaders(w, os.Getenv("ALLOWED_ORIGIN"))
    if r.Method == http.MethodOptions {
        w.WriteHeader(http.StatusNoContent)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{
        "status":      "ok",
        "commit":      CommitHash,
        "build_time":  BuildTime,
        "server_time": time.Now().UTC().Format(time.RFC3339),
    })
}

func parseConfig() Config {
    cfg := Config{
        HTTPPort:      getenvDefault("PORT", "8080"),
        UDPPort:       getenvDefault("UDP_PORT", "8081"),
        AllowedOrigin: getenvDefault("ALLOWED_ORIGIN", "*"),
    }
    // allow flags for local runs
    flag.StringVar(&cfg.HTTPPort, "port", cfg.HTTPPort, "HTTP port")
    flag.StringVar(&cfg.UDPPort, "udp", cfg.UDPPort, "UDP port")
    flag.StringVar(&cfg.AllowedOrigin, "origin", cfg.AllowedOrigin, "Allowed CORS origin")
    flag.Parse()
    return cfg
}

func getenvDefault(k, d string) string {
    if v := os.Getenv(k); v != "" {
        return v
    }
    return d
}

func splitTrim(s string, sep rune) []string {
    s = strings.TrimSpace(s)
    if s == "" {
        return nil
    }
    out := []string{}
    cur := strings.Builder{}
    for _, r := range s {
        if r == sep {
            out = append(out, strings.Trim(cur.String(), "/ "))
            cur.Reset()
        } else {
            cur.WriteRune(r)
        }
    }
    if cur.Len() > 0 {
        out = append(out, strings.Trim(cur.String(), "/ "))
    }
    // remove leading empty from paths like "/room/user"
    filtered := out[:0]
    for _, p := range out {
        if p != "" {
            filtered = append(filtered, p)
        }
    }
    return filtered
}

func main() {
    cfg := parseConfig()
    hub := NewHub()

    // HTTP routes
    http.HandleFunc("/health", healthHandler)
    http.HandleFunc("/ws", HandleWebSocket(hub, cfg.AllowedOrigin))

    // UDP relay
    if _, err := StartUDPRelay(cfg.UDPPort, hub); err != nil {
        log.Printf("UDP relay error: %v", err)
    } else {
        log.Printf("UDP relay listening on :%s", cfg.UDPPort)
    }

    addr := ":" + cfg.HTTPPort
    log.Printf("starting server on %s (commit=%s build=%s)", addr, CommitHash, BuildTime)
    srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
    if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        log.Fatalf("http server error: %v", err)
    }
}

