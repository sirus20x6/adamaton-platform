package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPProber_okOnLive2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	port := mustPort(t, srv.URL)
	res := NewHTTPProber(false).Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: port, Path: "/", Timeout: time.Second})
	if res.Status != StatusOK {
		t.Fatalf("status=%q detail=%q", res.Status, res.Detail)
	}
	if res.Stats["http_status"] != 200 {
		t.Fatalf("stats=%v", res.Stats)
	}
}

func TestHTTPProber_degradedOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	res := NewHTTPProber(false).Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: mustPort(t, srv.URL), Path: "/", Timeout: time.Second})
	if res.Status != StatusDegraded {
		t.Fatalf("status=%q", res.Status)
	}
}

func TestHTTPProber_offlineOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	res := NewHTTPProber(false).Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: mustPort(t, srv.URL), Path: "/", Timeout: time.Second})
	if res.Status != StatusOffline {
		t.Fatalf("status=%q", res.Status)
	}
}

func TestHTTPProber_offlineOnConnRefused(t *testing.T) {
	res := NewHTTPProber(false).Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: 1, Path: "/", Timeout: 250 * time.Millisecond})
	if res.Status != StatusOffline {
		t.Fatalf("status=%q detail=%q", res.Status, res.Detail)
	}
}

func TestTCPProber_ok(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	res := TCPProber{}.Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: port, Timeout: 500 * time.Millisecond})
	if res.Status != StatusOK {
		t.Fatalf("status=%q", res.Status)
	}
}

func TestTCPProber_offlineOnNoListener(t *testing.T) {
	res := TCPProber{}.Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: 1, Timeout: 250 * time.Millisecond})
	if res.Status != StatusOffline {
		t.Fatalf("status=%q detail=%q", res.Status, res.Detail)
	}
}

func TestRedisProber_okOnPong(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the PING frame to drain the buffer, then reply.
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("+PONG\r\n"))
	}()
	port := l.Addr().(*net.TCPAddr).Port
	res := RedisProber{}.Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: port, Timeout: time.Second})
	if res.Status != StatusOK {
		t.Fatalf("status=%q detail=%q", res.Status, res.Detail)
	}
}

func TestRedisProber_degradedOnGarbage(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("garbage from not-redis\r\n"))
	}()
	port := l.Addr().(*net.TCPAddr).Port
	res := RedisProber{}.Probe(context.Background(),
		Target{Host: "127.0.0.1"},
		Probe{Port: port, Timeout: time.Second})
	if res.Status != StatusDegraded {
		t.Fatalf("status=%q detail=%q", res.Status, res.Detail)
	}
}

func TestPostgresProber_nilPoolIsOffline(t *testing.T) {
	res := (&PostgresProber{Pool: nil}).Probe(context.Background(), Target{}, Probe{})
	if res.Status != StatusOffline {
		t.Fatalf("status=%q", res.Status)
	}
	if !strings.Contains(res.Detail, "no postgres pool") {
		t.Fatalf("detail=%q", res.Detail)
	}
}

func mustPort(t *testing.T, url string) int {
	t.Helper()
	// httptest URL looks like http://127.0.0.1:PORT
	idx := strings.LastIndex(url, ":")
	if idx < 0 {
		t.Fatalf("bad url: %s", url)
	}
	port, err := strconv.Atoi(url[idx+1:])
	if err != nil {
		t.Fatalf("port parse: %v", err)
	}
	return port
}
