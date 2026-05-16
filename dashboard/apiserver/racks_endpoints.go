// /api/v1/racks groups self-registered workers + declared sidecars by
// physical host so the Nodes UI can render one panel per rack. The
// rack list is embedded from apiserver/racks.yaml (a mirror of the
// per-host Adamaton/deploy/<host>/MANIFEST.yaml); the worker join
// against evo.workers happens at request time so liveness reflects
// the current state.
package apiserver

import (
	"context"
	_ "embed"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v3"
)

//go:embed racks.yaml
var racksYAML []byte

// rackManifest is the on-disk shape of one rack block in racks.yaml.
// JSON-omitted fields are kept off the wire so the frontend only sees
// the post-join Rack view.
type rackManifest struct {
	Host     string   `yaml:"host"`
	Aliases  []string `yaml:"aliases"`
	ImageTag string   `yaml:"image_tag"`
	Workers  []string `yaml:"workers"`
	Sidecars []string `yaml:"sidecars"`
	Services []string `yaml:"services"`
}

type rackManifestFile struct {
	Racks []rackManifest `yaml:"racks"`
}

// RackNode is one row in a rack's nodes[] array. Kind discriminates the
// downstream UI: "worker" rows carry a Worker; "sidecar" + "service"
// rows are declared-only and never have a Worker field.
type RackNode struct {
	Kind     string  `json:"kind"`    // worker | sidecar | service
	Service  string  `json:"service"` // compose service name
	WorkerID string  `json:"worker_id,omitempty"`
	Worker   *Worker `json:"worker,omitempty"`
	Status   string  `json:"status"` // active | offline | unknown
}

// Rack is the wire shape returned by /api/v1/racks. ImageTag mirrors
// deploy/<host>/MANIFEST.yaml so the UI can show "pi5 @ main" without
// a second fetch.
type Rack struct {
	Host     string     `json:"host"`
	ImageTag string     `json:"image_tag"`
	Aliases  []string   `json:"aliases,omitempty"`
	Nodes    []RackNode `json:"nodes"`
}

type racksResponse struct {
	Racks []Rack `json:"racks"`
}

// rackManifests is parsed once from the embedded yaml; safe to share
// across requests because the slices are never mutated after init.
var (
	rackManifestsOnce sync.Once
	rackManifestsVal  []rackManifest
	rackManifestsErr  error
)

func loadRackManifests() ([]rackManifest, error) {
	rackManifestsOnce.Do(func() {
		var f rackManifestFile
		rackManifestsErr = yaml.Unmarshal(racksYAML, &f)
		rackManifestsVal = f.Racks
	})
	return rackManifestsVal, rackManifestsErr
}

func (s *APIServer) registerRacksEndpoint(api *mux.Router) {
	api.HandleFunc("/racks", s.listRacks).Methods("GET")
}

func (s *APIServer) listRacks(w http.ResponseWriter, r *http.Request) {
	manifests, err := loadRackManifests()
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rack manifests: "+err.Error())
		return
	}

	// Pull every worker once; in-memory join below is faster than
	// per-rack SQL when the rack count grows. evoPool nil → workers
	// list stays empty; sidecars still render.
	workers := make([]Worker, 0)
	if s.evoPool != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rows, qerr := s.evoPool.Query(ctx, workersSelectSQL+` ORDER BY w.identity ASC`)
		if qerr != nil {
			writeEvoErr(w, http.StatusInternalServerError, "query: "+qerr.Error())
			return
		}
		for rows.Next() {
			wk, scanErr := scanWorker(rows)
			if scanErr != nil {
				rows.Close()
				writeEvoErr(w, http.StatusInternalServerError, "scan: "+scanErr.Error())
				return
			}
			workers = append(workers, wk)
		}
		rows.Close()
	}

	out := racksResponse{Racks: make([]Rack, 0, len(manifests))}
	for _, m := range manifests {
		aliases := append([]string{m.Host}, m.Aliases...)
		rack := Rack{
			Host:     m.Host,
			ImageTag: m.ImageTag,
			Aliases:  m.Aliases,
			Nodes:    make([]RackNode, 0, len(m.Workers)+len(m.Sidecars)+len(m.Services)),
		}
		for _, svc := range m.Workers {
			n := RackNode{Kind: "worker", Service: svc, Status: "unknown"}
			if wk := matchWorker(svc, aliases, workers); wk != nil {
				n.Worker = wk
				n.WorkerID = wk.ID
				n.Status = wk.Status
			}
			rack.Nodes = append(rack.Nodes, n)
		}
		for _, svc := range m.Sidecars {
			rack.Nodes = append(rack.Nodes, RackNode{Kind: "sidecar", Service: svc, Status: "unknown"})
		}
		for _, svc := range m.Services {
			rack.Nodes = append(rack.Nodes, RackNode{Kind: "service", Service: svc, Status: "unknown"})
		}
		out.Racks = append(out.Racks, rack)
	}
	writeEvoJSON(w, out)
}

// matchWorker finds the evo.workers row whose identity equals svc and
// whose ID's @suffix is in the rack's alias set. Returns nil if no
// match — the caller renders the node as kind=worker, status=unknown.
//
// Why identity AND suffix: identity alone is ambiguous when the same
// worker type runs on multiple hosts (nano-research-worker exists on
// both pi5 and pi5-speaker); the suffix is what disambiguates them.
func matchWorker(svc string, aliases []string, workers []Worker) *Worker {
	for i := range workers {
		wk := &workers[i]
		if wk.Identity != svc {
			continue
		}
		atIdx := strings.IndexByte(wk.ID, '@')
		if atIdx < 0 {
			continue
		}
		suffix := wk.ID[atIdx+1:]
		for _, a := range aliases {
			if suffix == a {
				return wk
			}
		}
	}
	return nil
}
