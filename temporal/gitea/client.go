// /thearray/gogents/internal/gitea/client.go
package gitea

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirus20x6/adamomaton-core/types"
)

// sanitizePathSegment escapes a user-supplied path segment to prevent path traversal
func sanitizePathSegment(s string) string {
	return url.PathEscape(s)
}

const (
	defaultGiteaTimeout = 30 * time.Second
	// errorBodyExcerptLen bounds the slice of an upstream error body that is
	// included in the returned error message. Anything longer is logged with
	// the full content but trimmed in the error string so log lines stay
	// scannable and don't leak large payloads through error wrapping.
	errorBodyExcerptLen = 256
)

// GiteaClient provides integration with self-hosted Gitea instances
type GiteaClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
	logger     *logrus.Logger
}

// NewGiteaClient creates a new Gitea API client.
//
// Connection-pool tuning: under the 12-agent fan-out the default
// http.Transport's MaxIdleConnsPerHost=2 forced ~10 of every 12 concurrent
// requests to redo a TCP+TLS handshake against Gitea. We install a custom
// transport with MaxIdleConnsPerHost=32 plus a TLS session cache so
// keep-alives and session resumption actually pay off. The transport is built
// for both secure and insecure modes — Insecure only flips InsecureSkipVerify
// on the same TLS config.
//
// The transport does NOT have an SSRF-aware DialContext: Gitea is a trusted
// upstream and the client only ever talks to the configured baseURL. The
// SSRF DialContext lives in internal/executor for the workflow-author-driven
// HTTP executor, which IS untrusted.
func NewGiteaClient(config types.GiteaConfig, logger *logrus.Logger) *GiteaClient {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultGiteaTimeout
	}

	httpClient := &http.Client{
		Timeout:   timeout,
		Transport: newTunedGiteaTransport(config.Insecure),
	}

	return &GiteaClient{
		baseURL:    strings.TrimSuffix(config.BaseURL, "/"),
		token:      config.Token,
		httpClient: httpClient,
		logger:     logger,
	}
}

// newTunedGiteaTransport returns an *http.Transport configured for high
// parallel fan-out against a single Gitea host:
//
//   - MaxIdleConnsPerHost=32 so 12 concurrent agents don't trample each
//     other into fresh handshakes.
//   - A TLS session cache so resumption survives idle pruning.
//   - MinVersion TLS 1.2 even when InsecureSkipVerify is on.
//   - HTTP/2 enabled (ForceAttemptHTTP2) so a single conn can multiplex.
//
// The ClientSessionCache size of 64 is generous for a single host but cheap;
// the cache only holds session tickets, not open connections.
func newTunedGiteaTransport(insecure bool) *http.Transport {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ClientSessionCache: tls.NewLRUClientSessionCache(64),
	}
	if insecure {
		tlsConfig.InsecureSkipVerify = true
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}
}

// Helper methods for HTTP requests

func (c *GiteaClient) makeRequest(ctx context.Context, method, path string, payload interface{}, result interface{}) error {
	req, err := c.buildRequest(ctx, method, path, payload)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	c.checkRateLimit(resp)

	// Treat non-2xx as errors (3xx redirects are not expected from Gitea API)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // 64KB limit on error body
		if readErr != nil {
			c.logger.WithError(readErr).Warn("Failed to read error response body")
		}

		excerpt := truncateForError(body)
		// Log the full body length and excerpt separately so operators can correlate
		// without the error message itself leaking the entire upstream payload.
		c.logger.WithFields(logrus.Fields{
			"status":       resp.StatusCode,
			"method":       method,
			"path":         path,
			"body_len":     len(body),
			"body_excerpt": excerpt,
		}).Error("Gitea API request failed")
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, excerpt)
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// truncateForError clips an arbitrary body to errorBodyExcerptLen so it can be
// safely embedded in an error message. The returned string is always valid UTF-8
// best-effort (we don't try to honor rune boundaries — error context, not display).
func truncateForError(body []byte) string {
	if len(body) <= errorBodyExcerptLen {
		return string(body)
	}
	return string(body[:errorBodyExcerptLen]) + "...[truncated]"
}

// checkRateLimit inspects Gitea's rate-limit response headers and emits a warning
// when the remaining quota for the token has been exhausted. It does not retry —
// retry policy is left to callers — but logging at zero gives operators a chance
// to spot tokens that are about to start failing.
func (c *GiteaClient) checkRateLimit(resp *http.Response) {
	remainingHdr := resp.Header.Get("X-RateLimit-Remaining")
	if remainingHdr == "" {
		return
	}
	remaining, err := strconv.Atoi(remainingHdr)
	if err != nil {
		return
	}
	if remaining <= 0 {
		c.logger.WithFields(logrus.Fields{
			"limit":     resp.Header.Get("X-RateLimit-Limit"),
			"reset":     resp.Header.Get("X-RateLimit-Reset"),
			"remaining": remaining,
		}).Warn("Gitea rate limit exhausted; subsequent requests will likely 429")
	}
}

func (c *GiteaClient) buildRequest(ctx context.Context, method, path string, payload interface{}) (*http.Request, error) {
	fullURL := c.baseURL + path

	var body io.Reader
	if payload != nil {
		jsonData, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}
		body = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GoGents/1.0")

	return req, nil
}
