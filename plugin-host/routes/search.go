// routes/search.go owns /platform/search/query (single-source GET) and
// POST /platform/search (multi-source fan-out used by the frontend's
// live-search panel). Both ultimately go through supervisor.EnsureRunning
// -> Plugin.SearchQuery; the fan-out handler aggregates per-plugin
// results into the legacy {hits, errors} shape callers expect.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	pluginv1 "github.com/sirus20x6/adamaton-platform/plugin-host/gen/go/dr/plugin/v1"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/supervisor"
)

func RegisterSearch(r *mux.Router, sup *supervisor.Supervisor, manifests map[string]*manifest.Manifest, logger *logrus.Logger) {
	r.HandleFunc("/platform/search/query", searchQueryHandler(sup, manifests, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/search", searchFanOutHandler(sup, manifests, logger)).Methods(http.MethodPost)
}

func searchQueryHandler(sup *supervisor.Supervisor, manifests map[string]*manifest.Manifest, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		source := q.Get("source")
		query := q.Get("q")
		if source == "" || query == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("source and q are required"))
			return
		}
		m, ok := manifests[source]
		if !ok {
			writeJSON(w, http.StatusNotFound, errorBody("unknown source"))
			return
		}
		if m.Category != "search" {
			writeJSON(w, http.StatusBadRequest, errorBody("plugin is not a search adapter"))
			return
		}

		limit := 10
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				writeJSON(w, http.StatusBadRequest, errorBody("limit must be 1..100"))
				return
			}
			limit = n
		}

		// 30s per call -- search plugins should be fast; the timeout
		// shields the host from a wedged adapter without bumping into
		// the typical UI debounce.
		ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()

		client, _, err := sup.EnsureRunning(ctx, source)
		if err != nil {
			if errors.Is(err, supervisor.ErrSpawnNotImplemented) {
				writeJSON(w, http.StatusNotImplemented, errorBody(
					"plugin supervisor spawn not yet implemented; search disabled until Phase C"))
				return
			}
			logger.WithError(err).WithField("source", source).Warn("ensure running")
			writeJSON(w, http.StatusBadGateway, errorBody("plugin unavailable"))
			return
		}

		resp, err := client.SearchQuery(ctx, &pluginv1.SearchQueryRequest{
			Query:  query,
			Limit:  int32(limit),
			Cursor: q.Get("cursor"),
			Since:  q.Get("since"),
		})
		if err != nil {
			logger.WithError(err).WithField("source", source).Warn("search query")
			writeJSON(w, http.StatusBadGateway, errorBody("search failed: "+err.Error()))
			return
		}
		// Pass page through as-is; the frontend already decodes protojson.
		writeJSON(w, http.StatusOK, map[string]any{
			"source": source,
			"page":   resp.GetPage(),
		})
	}
}

// searchFanOutRequest is the legacy {query, limit, sources?} body the
// frontend's liveSearch() sends. ``sources`` is optional — when absent
// we fan out to every search-category plugin.
type searchFanOutRequest struct {
	Query   string   `json:"query"`
	Limit   int      `json:"limit"`
	Sources []string `json:"sources"`
}

// searchFanOutHandler implements POST /platform/search: fan out a query
// across N search plugins in parallel, then aggregate hits + per-source
// errors into the shape RawSearchResponse on the frontend expects. The
// previous Python /platform/search did the same thing in-process; this
// handler is the strangler-fig replacement after the plugin-host migration.
func searchFanOutHandler(sup *supervisor.Supervisor, manifests map[string]*manifest.Manifest, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body searchFanOutRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid JSON body: "+err.Error()))
			return
		}
		if body.Query == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("query is required"))
			return
		}
		limit := body.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 100 {
			limit = 100
		}

		// Resolve the source set. Caller-specified ids are validated against
		// the manifest map; unknown ids land in the errors bag so callers see
		// why a source they asked for produced no hits.
		errs := map[string]string{}
		var sources []string
		if len(body.Sources) > 0 {
			for _, s := range body.Sources {
				m, ok := manifests[s]
				if !ok {
					errs[s] = "unknown source"
					continue
				}
				if m.Category != "search" {
					errs[s] = "plugin is not a search adapter"
					continue
				}
				sources = append(sources, s)
			}
		} else {
			for id, m := range manifests {
				if m.Category == "search" {
					sources = append(sources, id)
				}
			}
			sort.Strings(sources) // deterministic fan-out order for tests + logs
		}

		// 60s ceiling across the whole fan-out matches the frontend's UX
		// expectation; the per-plugin timeout below keeps a wedged adapter
		// from monopolising the budget.
		ctx, cancel := context.WithTimeout(req.Context(), 60*time.Second)
		defer cancel()

		type hit map[string]any
		var (
			mu   sync.Mutex
			hits []hit
		)
		var wg sync.WaitGroup
		for _, src := range sources {
			wg.Add(1)
			go func(src string) {
				defer wg.Done()
				perCallCtx, perCancel := context.WithTimeout(ctx, 30*time.Second)
				defer perCancel()

				client, _, err := sup.EnsureRunning(perCallCtx, src)
				if err != nil {
					mu.Lock()
					errs[src] = "spawn: " + err.Error()
					mu.Unlock()
					return
				}
				resp, err := client.SearchQuery(perCallCtx, &pluginv1.SearchQueryRequest{
					Query: body.Query,
					Limit: int32(limit),
				})
				if err != nil {
					mu.Lock()
					errs[src] = err.Error()
					mu.Unlock()
					return
				}
				page := resp.GetPage()
				if page == nil {
					return
				}
				mu.Lock()
				for _, r := range page.GetResults() {
					hits = append(hits, searchResultToHit(r))
				}
				mu.Unlock()
			}(src)
		}
		wg.Wait()

		// Stable hit ordering: highest score first, ties broken by adapter
		// name then external_id so reloads don't shuffle pills under
		// users mid-scroll.
		sort.SliceStable(hits, func(i, j int) bool {
			si, _ := hits[i]["score"].(float64)
			sj, _ := hits[j]["score"].(float64)
			if si != sj {
				return si > sj
			}
			ai, _ := hits[i]["adapter"].(string)
			aj, _ := hits[j]["adapter"].(string)
			if ai != aj {
				return ai < aj
			}
			ei, _ := hits[i]["external_id"].(string)
			ej, _ := hits[j]["external_id"].(string)
			return ei < ej
		})
		if len(hits) > limit*4 { // soft cap so one chatty adapter can't dominate
			hits = hits[:limit*4]
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"hits":   hits,
			"errors": errs,
		})
		if len(hits) == 0 {
			logger.WithFields(logrus.Fields{
				"query":   body.Query,
				"sources": sources,
				"errors":  errs,
			}).Info("search fan-out returned zero hits")
		}
	}
}

// searchResultToHit projects the protobuf SearchResult into the JSON
// shape the frontend RawHit consumer expects. Keeping this conversion
// explicit (vs handing back the protojson default) lets us coerce
// timestamps + handle the ``source_kind`` enum without leaking proto
// internals into the wire format.
func searchResultToHit(r *pluginv1.SearchResult) map[string]any {
	h := map[string]any{
		"adapter":     r.GetAdapter(),
		"external_id": r.GetExternalId(),
		"title":       r.GetTitle(),
		"url":         r.GetUrl(),
		"abstract":    r.GetAbstract(),
		"authors":     r.GetAuthors(),
		"venue":       r.GetVenue(),
		"score":       r.GetScore(),
	}
	if cc := r.GetCitationCount(); cc != 0 {
		h["citation_count"] = cc
	}
	if ts := r.GetPublishedAt(); ts != nil {
		h["published_at"] = ts.AsTime().UTC().Format(time.RFC3339)
	}
	if sk := r.GetSourceKind(); sk != pluginv1.SourceKind_SOURCE_KIND_UNSPECIFIED {
		h["source_kind"] = sk.String()
	}
	if raw := r.GetRaw(); raw != nil {
		h["raw"] = raw.AsMap()
	}
	return h
}
