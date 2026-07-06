package apiserver

// Short-lived terminal websocket tickets. The old flow put the long-lived API
// token in the ws URL (?token=...), where it lands in proxy logs and browser
// history. The hardened flow:
//
//  1. The SPA POSTs /api/v1/terminals/{sid}/ticket (normal header auth via
//     the /api/v1 middleware) and receives a single-purpose, HMAC-signed
//     ticket bound to that session id, valid for ~60s.
//  2. It opens the websocket with ?ticket=<ticket> (or the
//     "adam.ticket.<ticket>" subprotocol). The ticket is useless after
//     expiry and never equals the API token, so a leaked URL is inert.
//
// Non-browser clients can keep sending the API token via Authorization /
// X-API-Key headers on the handshake, or via the "adam.token.<token>"
// subprotocol. The legacy ?token=<api-token> query parameter still works but
// is DEPRECATED (logged) so the deployed SPA doesn't break during rollout.
//
// The signing secret is minted per process from crypto/rand: tickets don't
// need to survive an apiserver restart (the SPA just requests a new one when
// the ws drops).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

const (
	// terminalTicketTTL is how long a minted ticket is redeemable. Long
	// enough for the SPA to turn around and dial the ws, short enough that
	// a logged URL goes stale almost immediately.
	terminalTicketTTL = 60 * time.Second

	// Subprotocol prefixes accepted on the ws handshake. Browsers can set
	// Sec-WebSocket-Protocol (unlike Authorization), so these carry the
	// credential without touching the URL. The server selects the plain
	// "adam" subprotocol in its response when offered.
	wsSubprotoTokenPrefix  = "adam.token."
	wsSubprotoTicketPrefix = "adam.ticket."
	wsSubprotoAccept       = "adam"
)

// terminalTicketKey lazily mints the per-process HMAC key.
func (s *APIServer) terminalTicketKey() []byte {
	s.ticketKeyOnce.Do(func() {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			// rand.Read failing means the platform CSPRNG is broken; tickets
			// stay disabled (verify always fails) rather than signing with a
			// predictable key.
			s.logger.WithError(err).Error("terminal tickets: crypto/rand failed; ticket auth disabled")
			return
		}
		s.ticketKey = key
	})
	return s.ticketKey
}

// mintTerminalTicket signs sid+expiry: base64url(sid|exp) + "." +
// base64url(HMAC-SHA256(key, sid|exp)).
func (s *APIServer) mintTerminalTicket(sid string, now time.Time) (string, time.Time, bool) {
	key := s.terminalTicketKey()
	if len(key) == 0 {
		return "", time.Time{}, false
	}
	exp := now.Add(terminalTicketTTL)
	payload := sid + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	tkt := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return tkt, exp, true
}

// verifyTerminalTicket checks signature, session binding, and expiry.
func (s *APIServer) verifyTerminalTicket(ticket, sid string, now time.Time) bool {
	key := s.terminalTicketKey()
	if len(key) == 0 || ticket == "" {
		return false
	}
	dot := strings.LastIndexByte(ticket, '.')
	if dot <= 0 {
		return false
	}
	payloadB, err := base64.RawURLEncoding.DecodeString(ticket[:dot])
	if err != nil {
		return false
	}
	sigB, err := base64.RawURLEncoding.DecodeString(ticket[dot+1:])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payloadB)
	if !hmac.Equal(sigB, mac.Sum(nil)) {
		return false
	}
	payload := string(payloadB)
	bar := strings.LastIndexByte(payload, '|')
	if bar <= 0 {
		return false
	}
	if payload[:bar] != sid {
		return false
	}
	expUnix, err := strconv.ParseInt(payload[bar+1:], 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() <= expUnix
}

// issueTerminalTicket handles POST /terminals/{sid}/ticket. It sits behind
// the normal /api/v1 auth middleware, so possession of the API token (or a
// no-auth dev deployment) is what gates minting.
func (s *APIServer) issueTerminalTicket(w http.ResponseWriter, r *http.Request) {
	if !terminalsEnabled() {
		writeEvoErr(w, http.StatusServiceUnavailable, "terminals disabled (PTY_BACKEND=none)")
		return
	}
	sid := mux.Vars(r)["sid"]
	if strings.TrimSpace(sid) == "" {
		writeEvoErr(w, http.StatusBadRequest, "session id required")
		return
	}
	tkt, exp, ok := s.mintTerminalTicket(sid, time.Now())
	if !ok {
		writeEvoErr(w, http.StatusInternalServerError, "ticket signing unavailable")
		return
	}
	writeEvoJSON(w, map[string]any{
		"ticket":      tkt,
		"expires_at":  exp.UTC().Format(time.RFC3339),
		"ttl_seconds": int(terminalTicketTTL.Seconds()),
	})
}

// wsSubprotocolCredential scans the offered websocket subprotocols for an
// embedded token or ticket. Returns ("", "") when none offered.
func wsSubprotocolCredential(r *http.Request) (token, ticket string) {
	for _, p := range r.Header.Values("Sec-Websocket-Protocol") {
		for _, entry := range strings.Split(p, ",") {
			entry = strings.TrimSpace(entry)
			switch {
			case strings.HasPrefix(entry, wsSubprotoTokenPrefix):
				if token == "" {
					token = entry[len(wsSubprotoTokenPrefix):]
				}
			case strings.HasPrefix(entry, wsSubprotoTicketPrefix):
				if ticket == "" {
					ticket = entry[len(wsSubprotoTicketPrefix):]
				}
			}
		}
	}
	return token, ticket
}

// wsResponseHeader selects the "adam" subprotocol in the upgrade response
// when the client offered any adam.* entry. RFC 6455 requires the server to
// pick one of the offered subprotocols or none; browsers abort the
// connection when they offered subprotocols and the server picked none, so
// clients using the subprotocol credential MUST also offer plain "adam".
func wsResponseHeader(r *http.Request) http.Header {
	for _, p := range r.Header.Values("Sec-Websocket-Protocol") {
		for _, entry := range strings.Split(p, ",") {
			if strings.TrimSpace(entry) == wsSubprotoAccept {
				h := http.Header{}
				h.Set("Sec-Websocket-Protocol", wsSubprotoAccept)
				return h
			}
		}
	}
	return nil
}
