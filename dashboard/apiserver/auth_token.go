package apiserver

// API-token sourcing. Decision (2026-06-12): the projects/terminals/kanban
// auth credential lives in the core/credentialstore keyring; the apiserver
// loads it at boot (CREDENTIAL_ENCRYPTION_KEY bootstraps the ring), falling
// back to the API_TOKEN env / config token for compatibility.
//
// COMPATIBILITY: pi5's Caddy injects `Authorization: Bearer {env.EVO_API_TOKEN}`
// server-side, so existing deploys authenticate with the env token. When BOTH
// a keyring token and a (different) env token are present, both are accepted:
// the keyring token is the preferred credential going forward, the env token
// keeps deployed ingresses working until they're migrated. When only the
// keyring token exists it becomes THE token (config.API.Token), so the
// existing bind-address and startup-posture checks see auth as enabled.

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/credentialstore"
)

// apiTokenCredentialNameEnv overrides which credential row in the keyring
// holds the API token. Default: "evo-api-token".
const apiTokenCredentialNameEnv = "EVO_API_TOKEN_CREDENTIAL"

const defaultAPITokenCredentialName = "evo-api-token"

// credentialLister is the slice of *credentialstore.Store the loader uses;
// an interface so tests can stub the keyring without Postgres.
type credentialLister interface {
	List() ([]credentialstore.Credential, error)
	GetDecrypted(id string) (*credentialstore.Credential, string, error)
	Close() error
}

// loadKeyringAPIToken opens the credential keyring on the shared Postgres
// DSN and returns the decrypted API token, or "" when the keyring is
// unavailable (no DSN, no CREDENTIAL_ENCRYPTION_KEY, no matching credential).
// Every failure path is a soft fallback — the env token still works — but is
// logged so a keyring misconfiguration is visible.
func loadKeyringAPIToken(dsn string, logger *logrus.Logger) string {
	if dsn == "" {
		return ""
	}
	if os.Getenv("CREDENTIAL_ENCRYPTION_KEY") == "" && os.Getenv("CREDENTIAL_ENCRYPTION_KEYS") == "" {
		// The ring may already be cached process-wide from an earlier
		// NewStore; try anyway only if a store can be built. Cheap check:
		// without any key env AND without a cached ring, NewStore fails —
		// let it fail quietly at Debug below rather than warn-spam dev runs.
		logger.Debug("credential keyring: no CREDENTIAL_ENCRYPTION_KEY set; using API_TOKEN env only")
	}
	store, err := credentialstore.NewStoreWithLogger(dsn, logger)
	if err != nil {
		logger.WithError(err).Info("credential keyring unavailable; falling back to API_TOKEN env")
		return ""
	}
	defer func() { _ = store.Close() }()
	return keyringAPITokenFrom(store, logger)
}

// keyringAPITokenFrom finds the API-token credential by name and decrypts it.
// Split from loadKeyringAPIToken so tests can drive it with a fake lister.
func keyringAPITokenFrom(store credentialLister, logger *logrus.Logger) string {
	name := strings.TrimSpace(os.Getenv(apiTokenCredentialNameEnv))
	if name == "" {
		name = defaultAPITokenCredentialName
	}
	creds, err := store.List()
	if err != nil {
		logger.WithError(err).Info("credential keyring: list failed; falling back to API_TOKEN env")
		return ""
	}
	for _, c := range creds {
		if c.Name != name {
			continue
		}
		_, payload, err := store.GetDecrypted(c.ID)
		if err != nil {
			logger.WithError(err).WithField("credential", c.ID).
				Warn("credential keyring: decrypt failed for API token credential; falling back to API_TOKEN env")
			return ""
		}
		if tok := parseTokenPayload(payload); tok != "" {
			logger.WithField("credential", c.ID).Info("API token loaded from credential keyring")
			return tok
		}
		logger.WithField("credential", c.ID).
			Warn("credential keyring: API token credential payload empty/unparseable")
		return ""
	}
	logger.WithField("name", name).Debug("credential keyring: no API token credential found")
	return ""
}

// parseTokenPayload accepts either a raw token string or a JSON object with
// a "token" (or "value" / "api_key") field — the credential UI stores JSON.
func parseTokenPayload(payload string) string {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return ""
	}
	if strings.HasPrefix(payload, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err == nil {
			for _, k := range []string{"token", "value", "api_key", "apiKey"} {
				if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
		return ""
	}
	return payload
}

// installKeyringToken merges a keyring-sourced token into the server's
// accepted-token set. Called from NewAPIServer after config load.
func (s *APIServer) installKeyringToken(keyringToken string) {
	keyringToken = strings.TrimSpace(keyringToken)
	if keyringToken == "" {
		return
	}
	switch {
	case s.config.API.Token == "":
		// Keyring is the only credential: it becomes THE token so the
		// bind-address validation and auth-posture warning treat auth as on.
		s.config.API.Token = keyringToken
	case s.config.API.Token != keyringToken:
		// Both configured and different: accept both. Keyring is preferred
		// going forward; the env token keeps Caddy's injected header working.
		s.extraAPITokens = append(s.extraAPITokens, keyringToken)
		s.logger.Info("API auth: keyring token active alongside env token (env kept for ingress compatibility)")
	}
}

// authTokenConfigured reports whether ANY API token is configured.
func (s *APIServer) authTokenConfigured() bool {
	return s.config.API.Token != "" || len(s.extraAPITokens) > 0
}

// validAPIToken constant-time-compares the presented token against every
// configured token (config/env token + keyring extras).
func (s *APIServer) validAPIToken(token string) bool {
	ok := false
	if s.config.API.Token != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(s.config.API.Token)) == 1 {
		ok = true
	}
	for _, t := range s.extraAPITokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 {
			ok = true
		}
	}
	return ok
}

// requestHeaderToken extracts the presented token from X-API-Key or a
// Bearer Authorization header (shared by the HTTP middleware and the
// websocket handshake check).
func requestHeaderToken(r *http.Request) string {
	if token := strings.TrimSpace(r.Header.Get("X-API-Key")); token != "" {
		return token
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	return ""
}
