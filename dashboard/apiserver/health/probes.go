package health

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Status mirrors the SPA's pill colours: ok | degraded | offline | unknown.
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
	StatusOffline  Status = "offline"
	StatusUnknown  Status = "unknown"
)

// Target identifies one concrete instance to probe. Host is the
// docker-network DNS name (or LAN IP for cross-host services like
// vLLM); HostLabel is the human host name ("pi5"). Container is the
// docker container name; empty when not applicable (e.g. postgres
// probe goes through pgxpool, not a container).
type Target struct {
	Host      string // probe address — "r2g", "10.0.4.37", etc.
	HostLabel string // user-facing host name — "pi5"
	Container string
	Image     string
}

// Result is what a Probe returns. Status + a short Detail when not ok.
type Result struct {
	Status    Status
	Detail    string
	LatencyMS float64
	// Stats is optional — kinds that want to surface metric values
	// (e.g. http probe returning the response status, temporal_queue
	// returning heartbeat age) populate it.
	Stats map[string]any
}

// Prober is implemented by per-kind probes. Each Kind has one impl.
type Prober interface {
	Probe(ctx context.Context, target Target, p Probe) Result
}

// HTTPProber GETs a configured path; 2xx -> ok, 4xx -> degraded, 5xx +
// transport errors -> offline.
type HTTPProber struct {
	Client *http.Client
}

// NewHTTPProber builds a Prober with an http.Client that does NOT
// re-verify the host's TLS cert when the operator opted in via
// EVO_DASHBOARD_TLS_INSECURE. The apiserver already keeps a singleton
// shaped this way for the deepresearch probe; we mirror it here so
// callers don't need to share globals.
func NewHTTPProber(insecureTLS bool) *HTTPProber {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},
	}
	return &HTTPProber{
		Client: &http.Client{Transport: tr, Timeout: 10 * time.Second},
	}
}

func (h *HTTPProber) Probe(ctx context.Context, target Target, p Probe) Result {
	start := time.Now()
	if p.Port == 0 || p.Path == "" {
		return offline(start, "probe missing port/path")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scheme := "http"
	if p.Port == 443 {
		scheme = "https"
	}
	host := p.Host
	if host == "" {
		host = target.Host
	}
	url := fmt.Sprintf("%s://%s:%d%s", scheme, host, p.Port, p.Path)
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return offline(start, err.Error())
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return offline(start, err.Error())
	}
	defer resp.Body.Close()
	r := Result{
		LatencyMS: msSince(start),
		Stats:     map[string]any{"http_status": resp.StatusCode},
	}
	switch {
	case resp.StatusCode >= 500:
		r.Status = StatusOffline
		r.Detail = resp.Status
	case resp.StatusCode >= 400:
		r.Status = StatusDegraded
		r.Detail = resp.Status
	default:
		r.Status = StatusOK
	}
	return r
}

// TCPProber does net.DialTimeout; connect = ok, anything else = offline.
type TCPProber struct{}

func (TCPProber) Probe(ctx context.Context, target Target, p Probe) Result {
	start := time.Now()
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	host := p.Host
	if host == "" {
		host = target.Host
	}
	dialer := net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(host, strconv.Itoa(p.Port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return offline(start, err.Error())
	}
	_ = conn.Close()
	return Result{Status: StatusOK, LatencyMS: msSince(start)}
}

// RedisProber speaks the bare minimum of RESP: send PING, expect +PONG.
// Avoids pulling go-redis just for liveness.
type RedisProber struct{}

func (RedisProber) Probe(ctx context.Context, target Target, p Probe) Result {
	start := time.Now()
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	host := p.Host
	if host == "" {
		host = target.Host
	}
	dialer := net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(host, strconv.Itoa(p.Port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return offline(start, err.Error())
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return offline(start, "write: "+err.Error())
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return offline(start, "read: "+err.Error())
	}
	if len(line) < 5 || line[0] != '+' || line[1:5] != "PONG" {
		return Result{
			Status:    StatusDegraded,
			Detail:    "unexpected RESP: " + line,
			LatencyMS: msSince(start),
		}
	}
	return Result{Status: StatusOK, LatencyMS: msSince(start)}
}

// PostgresProber pings the shared evoPool. Doesn't take a Target — the
// apiserver only knows about one postgres pool. Stats reports the
// stat tracker's view of the pool (acquired / idle).
type PostgresProber struct {
	Pool *pgxpool.Pool
}

func (pp *PostgresProber) Probe(ctx context.Context, _ Target, p Probe) Result {
	start := time.Now()
	if pp.Pool == nil {
		return offline(start, "no postgres pool configured")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pp.Pool.Ping(pingCtx); err != nil {
		return offline(start, err.Error())
	}
	stat := pp.Pool.Stat()
	return Result{
		Status:    StatusOK,
		LatencyMS: msSince(start),
		Stats: map[string]any{
			"acquired": stat.AcquiredConns(),
			"idle":     stat.IdleConns(),
			"max":      stat.MaxConns(),
			"total":    stat.TotalConns(),
		},
	}
}

// TemporalQueueProber counts non-stale workers in evo.workers that
// declare the queue. healthy = workers with last_heartbeat within
// HeartbeatMaxAge. Status is set against MinHealthy by the
// aggregator's role rollup, not here — this prober only reports the
// raw counts.
type TemporalQueueProber struct {
	Pool *pgxpool.Pool
}

// QueueLiveness is what TemporalQueueProber returns via Result.Stats.
// Keys: workers_alive, workers_total, oldest_heartbeat_age_ms.
type QueueLiveness struct {
	Alive              int
	Total              int
	OldestHeartbeatAge time.Duration
}

func (tp *TemporalQueueProber) Probe(ctx context.Context, _ Target, p Probe) Result {
	// NB: this signature is unused — temporal_queue probes go through
	// ProbeQueue below since they need the role's Queue + HeartbeatMaxAge
	// fields rather than the generic Probe struct. Kept here so the
	// type implements Prober for symmetry.
	_ = p
	return Result{Status: StatusUnknown, Detail: "use ProbeQueue for temporal_queue"}
}

// ProbeQueue is the temporal_queue entry point. Returns count of
// "alive" workers (heartbeat within maxAge) + total workers declaring
// the queue + the oldest still-alive heartbeat age (for stats).
func (tp *TemporalQueueProber) ProbeQueue(ctx context.Context, queue string, maxAge time.Duration) (QueueLiveness, error) {
	if tp.Pool == nil {
		return QueueLiveness{}, fmt.Errorf("no postgres pool")
	}
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	const q = `
		SELECT
			count(*) FILTER (WHERE last_heartbeat > NOW() - $2::interval) AS alive,
			count(*) AS total,
			COALESCE(EXTRACT(EPOCH FROM (NOW() - max(last_heartbeat))) * 1000, 0) AS oldest_ms
		FROM evo.workers
		WHERE $1 = ANY(declared_queues)
	`
	var alive, total int
	var oldestMS float64
	err := tp.Pool.QueryRow(queryCtx, q, queue, maxAge.String()).Scan(&alive, &total, &oldestMS)
	if err != nil {
		return QueueLiveness{}, err
	}
	return QueueLiveness{
		Alive:              alive,
		Total:              total,
		OldestHeartbeatAge: time.Duration(oldestMS) * time.Millisecond,
	}, nil
}

func offline(start time.Time, detail string) Result {
	return Result{
		Status:    StatusOffline,
		Detail:    detail,
		LatencyMS: msSince(start),
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
