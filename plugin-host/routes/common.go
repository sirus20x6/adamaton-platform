// Package routes hosts the HTTP handlers plugin-host exposes under
// /platform/*. Shared helpers live here so each route file can stay
// focused on its endpoint group.
package routes

import (
	"encoding/json"
	"net/http"

	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
)

// writeJSON sets the Content-Type, writes the status, encodes the body.
// Encoding errors after WriteHeader can't be reported structurally so we
// drop them on the floor; the access log captures the 200/!=200 split.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// errorBody is the canonical error envelope; matches r2g's shape so the
// frontend has one JSON contract across both backends.
func errorBody(msg string) map[string]any {
	return map[string]any{"ok": false, "error": msg}
}

// manifestPayload is the JSON projection the frontend sees. id+name+
// description+category+capabilities+icon+version+args_schema is the
// stable subset the plugin grid relies on; ConfigSchema deliberately
// stays off the list endpoint to keep the response small.
func manifestPayload(m *manifest.Manifest) map[string]any {
	return map[string]any{
		"id":           m.ID,
		"name":         m.Name,
		"description":  m.Description,
		"category":     m.Category,
		"capabilities": m.Capabilities,
		"icon":         m.Icon,
		"version":      m.Version,
		"args_schema":  m.ArgsSchema,
	}
}
