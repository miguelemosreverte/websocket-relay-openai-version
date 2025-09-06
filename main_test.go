package main

import (
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "sync"
    "testing"
    "time"

    "github.com/gorilla/websocket"
)

var (
    reportJSON string
    reportMD   string
    duration   time.Duration
    clients    int
    baseURL    string
)

func init() {
    flag.StringVar(&reportJSON, "report.json", "", "Path to write JSON report (optional)")
    flag.StringVar(&reportMD, "report.md", "", "Path to write Markdown report (optional)")
    flag.DurationVar(&duration, "duration", 1500*time.Millisecond, "Functional/benchmark duration")
    flag.IntVar(&clients, "clients", 3, "Number of websocket clients")
    flag.StringVar(&baseURL, "baseURL", "", "Base URL to target existing server (optional, e.g., https://domain)")
}

type tsPoint struct {
    Second          int64   `json:"second"`
    Sent            int     `json:"sent"`
    Received        int     `json:"received"`
    AvgLatencyMs    float64 `json:"avg_latency_ms"`
    BytesSent       int64   `json:"bytes_sent"`
    BytesReceived   int64   `json:"bytes_received"`
}

type BenchResult struct {
    Commit       string     `json:"commit"`
    BuildTime    string     `json:"build_time"`
    StartedAt    time.Time  `json:"started_at"`
    DurationSec  float64    `json:"duration_sec"`
    Room         string     `json:"room"`
    Clients      int        `json:"clients"`
    TotalSent    int        `json:"total_sent"`
    TotalRecv    int        `json:"total_received"`
    AvgLatencyMs float64    `json:"avg_latency_ms"`
    P50Ms        float64    `json:"p50_ms"`
    P95Ms        float64    `json:"p95_ms"`
    BytesSent    int64      `json:"bytes_sent"`
    BytesRecv    int64      `json:"bytes_received"`
    Series       []tsPoint  `json:"series"`
}

func TestFunctional(t *testing.T) {
    var wsURL string
    if baseURL == "" {
        // Spin up in-process HTTP server
        hub := NewHub()
        mux := http.NewServeMux()
        mux.HandleFunc("/health", healthHandler)
        mux.HandleFunc("/ws", HandleWebSocket(hub, "*"))
        ts := httptest.NewServer(mux)
        defer ts.Close()
        wsURL = "ws" + ts.URL[len("http"):]
    } else {
        if baseURL[:4] == "http" {
            wsURL = "ws" + baseURL[len("http"):]
        } else if baseURL[:2] == "ws" {
            wsURL = baseURL
        } else {
            wsURL = "wss://" + baseURL
        }
    }

    ctx, cancel := context.WithTimeout(context.Background(), duration)
    defer cancel()

    // Connect N clients
    type stats struct {
        sent, recv int
        bytesSent, bytesRecv int64
        latMu   sync.Mutex
        lats    []float64
        series  []tsPoint
    }
    st := &stats{}
    wg := sync.WaitGroup{}

    // per-second aggregation
    tickStart := time.Now()
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()
    go func() {
        lastSent, lastRecv := 0, 0
        lastBytesSent, lastBytesRecv := int64(0), int64(0)
        for {
            select {
            case <-ticker.C:
                sec := time.Since(tickStart).Truncate(time.Second).Seconds()
                st.latMu.Lock()
                var avg float64
                if len(st.lats) > 0 {
                    var sum float64
                    for _, v := range st.lats {
                        sum += v
                    }
                    avg = sum / float64(len(st.lats))
                    st.lats = nil
                }
                st.series = append(st.series, tsPoint{
                    Second:       int64(sec),
                    Sent:         st.sent - lastSent,
                    Received:     st.recv - lastRecv,
                    AvgLatencyMs: avg,
                    BytesSent:    st.bytesSent - lastBytesSent,
                    BytesReceived: st.bytesRecv - lastBytesRecv,
                })
                lastSent, lastRecv = st.sent, st.recv
                lastBytesSent, lastBytesRecv = st.bytesSent, st.bytesRecv
                st.latMu.Unlock()
            case <-ctx.Done():
                return
            }
        }
    }()

    for i := 0; i < clients; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            d := websocket.Dialer{}
            room := "global"
            user := fmt.Sprintf("u%d", id)
            c, _, err := d.Dial(fmt.Sprintf("%s/ws/%s/%s", wsURL, room, user), nil)
            if err != nil {
                t.Errorf("dial err: %v", err)
                return
            }
            defer c.Close()

            // reader
            done := make(chan struct{})
            go func() {
                defer close(done)
                for {
                    c.SetReadDeadline(time.Now().Add(5 * time.Second))
                    _, msg, err := c.ReadMessage()
                    if err != nil {
                        return
                    }
                    // envelope ts included by server; approximate latency
                    var env Envelope
                    _ = json.Unmarshal(msg, &env)
                    lat := float64(time.Now().UnixNano()-env.Ts) / 1e6
                    st.latMu.Lock()
                    st.recv++
                    st.bytesRecv += int64(len(env.Payload))
                    st.lats = append(st.lats, lat)
                    st.latMu.Unlock()
                }
            }()

            // writer
            payload := []byte("hello")
            for {
                select {
                case <-ctx.Done():
                    return
                default:
                    c.SetWriteDeadline(time.Now().Add(2 * time.Second))
                    if err := c.WriteMessage(websocket.BinaryMessage, payload); err != nil {
                        return
                    }
                    st.latMu.Lock()
                    st.sent++
                    st.bytesSent += int64(len(payload))
                    st.latMu.Unlock()
                    time.Sleep(20 * time.Millisecond)
                }
            }
        }(i)
    }

    wg.Wait()

    // compute percentiles
    st.latMu.Lock()
    lats := make([]float64, 0)
    for _, s := range st.series {
        if s.AvgLatencyMs > 0 {
            lats = append(lats, s.AvgLatencyMs)
        }
    }
    st.latMu.Unlock()

    p50, p95 := percentile(lats, 50), percentile(lats, 95)
    avg := mean(lats)

    res := BenchResult{
        Commit:       CommitHash,
        BuildTime:    BuildTime,
        StartedAt:    time.Now().Add(-duration),
        DurationSec:  duration.Seconds(),
        Room:         "global",
        Clients:      clients,
        TotalSent:    st.sent,
        TotalRecv:    st.recv,
        AvgLatencyMs: avg,
        P50Ms:        p50,
        P95Ms:        p95,
        BytesSent:    st.bytesSent,
        BytesRecv:    st.bytesRecv,
        Series:       st.series,
    }

    t.Logf("sent=%d recv=%d avg=%.2fms p50=%.2fms p95=%.2fms", res.TotalSent, res.TotalRecv, res.AvgLatencyMs, res.P50Ms, res.P95Ms)

    if reportJSON != "" {
        if err := writeJSON(reportJSON, res); err != nil {
            t.Fatalf("write json: %v", err)
        }
    }
    if reportMD != "" {
        if err := writeMarkdown(reportMD, res); err != nil {
            t.Fatalf("write md: %v", err)
        }
    }
}

func writeJSON(path string, v any) error {
    if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
        return err
    }
    f, err := os.Create(path)
    if err != nil { return err }
    defer f.Close()
    enc := json.NewEncoder(f)
    enc.SetIndent("", "  ")
    return enc.Encode(v)
}

func writeMarkdown(path string, r BenchResult) error {
    if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
        return err
    }
    f, err := os.Create(path)
    if err != nil { return err }
    defer f.Close()
    _, err = fmt.Fprintf(f, "# Benchmark Result\n\n")
    if err != nil { return err }
    _, err = fmt.Fprintf(f, "- Commit: %s\n- Build: %s\n- Duration: %.1fs\n- Clients: %d\n- Sent: %d\n- Received: %d\n- Avg Latency: %.2f ms\n- P50: %.2f ms\n- P95: %.2f ms\n- Bytes Sent: %d\n- Bytes Received: %d\n\n",
        r.Commit, r.BuildTime, r.DurationSec, r.Clients, r.TotalSent, r.TotalRecv, r.AvgLatencyMs, r.P50Ms, r.P95Ms, r.BytesSent, r.BytesRecv)
    if err != nil { return err }
    _, err = fmt.Fprintf(f, "## Time Series (per second)\n\nSecond | Sent | Received | Avg Latency (ms) | Bytes Sent | Bytes Received\n---|---:|---:|---:|---:|---:\n")
    if err != nil { return err }
    for _, p := range r.Series {
        _, err = fmt.Fprintf(f, "%d | %d | %d | %.2f | %d | %d\n", p.Second, p.Sent, p.Received, p.AvgLatencyMs, p.BytesSent, p.BytesReceived)
        if err != nil { return err }
    }
    return nil
}

func percentile(vals []float64, p float64) float64 {
    if len(vals) == 0 { return 0 }
    cp := append([]float64(nil), vals...)
    sortFloats(cp)
    idx := int((p/100.0)*float64(len(cp)-1) + 0.5)
    if idx < 0 { idx = 0 }
    if idx >= len(cp) { idx = len(cp)-1 }
    return cp[idx]
}

func mean(vals []float64) float64 {
    if len(vals) == 0 { return 0 }
    var s float64
    for _, v := range vals { s += v }
    return s / float64(len(vals))
}

func sortFloats(a []float64) {
    // simple insertion sort to avoid dependencies
    for i := 1; i < len(a); i++ {
        v := a[i]
        j := i - 1
        for j >= 0 && a[j] > v {
            a[j+1] = a[j]
            j--
        }
        a[j+1] = v
    }
}
