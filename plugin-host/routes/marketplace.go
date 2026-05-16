// routes/marketplace.go scaffolds /platform/marketplace/* -- same shape
// as importers, filtered to category=marketplace. No live adapters this
// round; the route exists so the frontend's marketplace tab can boot
// against an empty list instead of a 404.
package routes

import (
	"net/http"

	"github.com/gorilla/mux"

	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
)

func RegisterMarketplace(r *mux.Router, manifests map[string]*manifest.Manifest) {
	r.HandleFunc("/platform/marketplace/", listMarketplaceHandler(manifests)).Methods(http.MethodGet)
	r.HandleFunc("/platform/marketplace", listMarketplaceHandler(manifests)).Methods(http.MethodGet)
}

func listMarketplaceHandler(manifests map[string]*manifest.Manifest) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		items := make([]map[string]any, 0)
		for _, m := range manifests {
			if m.Category != "marketplace" {
				continue
			}
			items = append(items, manifestPayload(m))
		}
		writeJSON(w, http.StatusOK, map[string]any{"plugins": items})
	}
}
