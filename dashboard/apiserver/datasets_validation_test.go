// Input-validation tests for the datasets create/import endpoints. The
// pure validators (validateDatasetID / capLen) need no infra; the
// handler-level 400 assertions construct an APIServer with the shared DB
// pool (createDataset 503s on a nil pool BEFORE it validates, so we need a
// real pool to reach the validation branch — but the bad input is rejected
// before any INSERT runs, so no row is written).
package apiserver

import (
	"net/http"
	"strings"
	"testing"
)

func TestValidateDatasetID(t *testing.T) {
	good := []string{"ds1", "my-dataset", "a.b_c-1", "A", "kernel.bench.v2"}
	for _, id := range good {
		if err := validateDatasetID(id); err != nil {
			t.Errorf("validateDatasetID(%q) should pass, got: %v", id, err)
		}
	}
	bad := map[string]string{
		"empty":         "",
		"slash":         "a/b",
		"leading-dash":  "-bad",
		"leading-dot":   ".bad",
		"space":         "a b",
		"semicolon":     "a;b",
		"newline":       "a\nb",
		"too-long":      strings.Repeat("a", datasetIDMaxLen+1),
		"control":       "a\tb",
		"path-traverse": "../escape",
	}
	for name, id := range bad {
		if err := validateDatasetID(id); err == nil {
			t.Errorf("validateDatasetID(%s=%q) should fail", name, id)
		}
	}
}

func TestCapLen(t *testing.T) {
	if err := capLen("f", "short", 10); err != nil {
		t.Errorf("within cap should pass: %v", err)
	}
	if err := capLen("f", strings.Repeat("x", 11), 10); err == nil {
		t.Error("over cap should fail")
	}
}

func TestCreateDataset_RejectsBadID(t *testing.T) {
	s := newDBTestServer(t) // skips if DB unavailable
	rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets",
		`{"id":"bad/id","task_type":"sft"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad id should 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "id") {
		t.Errorf("error body should mention id: %s", rr.Body.String())
	}
}

func TestCreateDataset_RejectsOversizedDisplayName(t *testing.T) {
	s := newDBTestServer(t)
	big := strings.Repeat("x", datasetDisplayMaxLen+1)
	rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets",
		`{"id":"okid","display_name":"`+big+`","task_type":"sft"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized display_name should 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateDataset_RejectsBadTaskType(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets",
		`{"id":"okid","task_type":"not-a-type"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad task_type should 400, got %d", rr.Code)
	}
}

// importDataset 503s on a nil temporalClient before validating, so the
// validation branch is asserted via the pure validator above plus this
// test which confirms a nil-temporal server still 503s (no panic) and that
// the validators are wired. With a configured temporal client absent we
// can't reach the 400 path through the handler, so this just guards the
// 503 contract.
func TestImportDataset_NoTemporal503(t *testing.T) {
	s := newPoollessServer(t) // temporalClient is nil
	rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets/import",
		`{"dataset_id":"okid","source_kind":"local","source_ref":"/data"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("import with no temporal should 503, got %d: %s", rr.Code, rr.Body.String())
	}
}
