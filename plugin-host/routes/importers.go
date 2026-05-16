// routes/importers.go owns /platform/importers/* -- a category-filtered
// view of /platform/plugins. The legacy Python /platform/importers
// endpoint stayed separate so the frontend's Sources tab could rely on
// "everything here is an importer"; we preserve that contract.
package routes

import (
	"net/http"

	"github.com/gorilla/mux"

	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
)

func RegisterImporters(r *mux.Router, manifests map[string]*manifest.Manifest) {
	r.HandleFunc("/platform/importers/", listImportersHandler(manifests)).Methods(http.MethodGet)
	r.HandleFunc("/platform/importers", listImportersHandler(manifests)).Methods(http.MethodGet)
}

func listImportersHandler(manifests map[string]*manifest.Manifest) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		items := make([]map[string]any, 0)
		for _, m := range manifests {
			if m.Category != "importer" {
				continue
			}
			items = append(items, manifestPayload(m))
		}
		writeJSON(w, http.StatusOK, map[string]any{"plugins": items})
	}
}
