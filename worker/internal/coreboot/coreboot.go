// Package coreboot factors the small env-reading, DSN-redacting,
// and Temporal-dial-with-retry helpers that the standalone workers
// (skills, reindex, dispatch, evo) each used to define inline. The
// unified adamaton-worker pulls them from here so the per-queue
// setup files in cmd/adamaton-worker/ stay short.
//
// Kept under internal/ because nothing outside the worker module
// should depend on these helpers — the same logic is duplicated in
// each standalone cmd/<worker>/main.go for now, and we don't want
// random callers reaching into platform/worker to grab them.
package coreboot

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"
)

// EnvOr returns os.LookupEnv(key) if set and non-empty, otherwise
// fallback. Mirrors the helper every standalone worker had inline.
func EnvOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// EnvInt reads an integer env var with a fallback. A non-integer or
// non-positive value is treated as unset so a misconfigured env
// can't silently floor concurrency to zero. Mirrors the reindex-worker
// pattern that justified the per-queue MaxConcurrentActivityExecutionSize
// tuning.
func EnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// EnvBool returns true for "1" / "true" / "yes"; anything else
// (including unset) returns false. Mirrors the SKILLS_R2R_INSECURE
// parsing in dashboard/apiserver/skills_r2r_sync.go.
func EnvBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

// ResolveDSN returns the first non-empty value among envVars, or
// fallback if none are set. Each queue's Postgres DSN can come from
// a queue-specific env (REINDEX_POSTGRES_DSN), an evo-shared env
// (EVO_POSTGRES_DSN), or a generic POSTGRES_DSN — the fallback chain
// matters because the four standalone workers each chose a different
// preferred name.
func ResolveDSN(envVars []string, fallback string) string {
	for _, ev := range envVars {
		if v := os.Getenv(ev); v != "" {
			return v
		}
	}
	return fallback
}

// RedactDSN strips the password segment from a Postgres DSN before
// logging it. Byte-scan based so the literal "***" lands in the
// output (url.URL.String() would URL-encode the asterisks). Returns
// the input unchanged if there is no password to redact.
func RedactDSN(dsn string) string {
	at := -1
	colon := -1
	for i := 0; i < len(dsn); i++ {
		c := dsn[i]
		if c == '@' {
			at = i
			break
		}
		// Skip the scheme separator at "://". The "11" guard mirrors
		// the standalone skills-worker's redactDSN — it sidesteps the
		// scheme colon for any postgres / postgresql / mysql URL that
		// begins with a scheme of at most ~10 chars.
		if c == ':' && i > 11 {
			colon = i
		}
	}
	if at < 0 || colon < 0 || colon >= at {
		return dsn
	}
	return dsn[:colon+1] + "***" + dsn[at:]
}

// DialTemporalWithRetry retries client.Dial with linear backoff
// capped at 30s. Honours ctx cancellation so a SIGINT during startup
// exits cleanly instead of waiting out the sleep. Each standalone
// worker had its own copy of this — under s6-overlay each queue
// process gets its own retry loop, and a temporal server still
// warming up at container boot doesn't trigger a restart-storm.
func DialTemporalWithRetry(ctx context.Context, addr, namespace, identity string, logger *logrus.Logger) (client.Client, error) {
	const (
		base    = 5 * time.Second
		maxStep = 6
	)
	var attempts atomic.Int32
	for {
		c, err := client.Dial(client.Options{
			HostPort:  addr,
			Namespace: namespace,
			Identity:  identity,
		})
		if err == nil {
			if a := attempts.Load(); a > 0 {
				logger.WithField("attempts", a).Info("temporal connection established after retries")
			}
			return c, nil
		}
		a := attempts.Add(1)
		step := int32(a)
		if step > maxStep {
			step = maxStep
		}
		wait := base * time.Duration(step)
		logger.WithError(err).Warnf("temporal dial failed; retrying in %s", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}
