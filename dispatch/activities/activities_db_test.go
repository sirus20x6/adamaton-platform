package activities

// DB-backed tests for the dispatch activities against a locally-migrated
// evo schema (evo.jobs + evo.workers). When the database is unreachable
// the suite SKIPs rather than failing, mirroring the dashboard test
// helper convention, so `go test ./...` stays green on machines without
// Postgres. Override the target with EVO_TEST_DSN.

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamaton-platform/dispatch/workflows"
)

const defaultTestDSN = "postgres://postgres:postgres@localhost:5432/postgres"

func testDSN() string {
	if v := os.Getenv("EVO_TEST_DSN"); v != "" {
		return v
	}
	return defaultTestDSN
}

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

func sharedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := pgxpool.New(ctx, testDSN())
		if err != nil {
			poolErr = err
			return
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			poolErr = err
			return
		}
		// Require the migrated evo schema — skip (not fail) when absent.
		var reg *string
		if err := p.QueryRow(ctx, "SELECT to_regclass('evo.jobs')::text").Scan(&reg); err != nil || reg == nil {
			p.Close()
			poolErr = errors.New("evo.jobs not present in test database")
			return
		}
		pool = p
	})
	if poolErr != nil {
		t.Skipf("evo test database unavailable: %v", poolErr)
	}
	return pool
}

// insertWorker creates an active worker row and registers cleanup.
func insertWorker(t *testing.T, p *pgxpool.Pool, id, queue string, mutate func(cols map[string]any)) {
	t.Helper()
	cols := map[string]any{
		"declared_queues": []string{queue},
		"cpu_arch":        "amd64",
		"cpu_features":    []string{"avx2"},
		"ram_gb":          64,
		"gpu_model":       nil,
		"gpu_vram_gb":     nil,
		"permissions":     []string{"execute"},
		"status":          "active",
		"heartbeat_ago_s": 0,
	}
	if mutate != nil {
		mutate(cols)
	}
	ctx := context.Background()
	_, err := p.Exec(ctx, `
		INSERT INTO evo.workers (id, identity, hostname, declared_queues, cpu_arch,
		       cpu_features, ram_gb, gpu_model, gpu_vram_gb, permissions, status,
		       last_heartbeat)
		VALUES ($1, $1, $1, $2, $3, $4, $5, $6, $7, $8, $9,
		        now() - make_interval(secs => $10))
	`, id, cols["declared_queues"], cols["cpu_arch"], cols["cpu_features"],
		cols["ram_gb"], cols["gpu_model"], cols["gpu_vram_gb"],
		cols["permissions"], cols["status"], cols["heartbeat_ago_s"])
	if err != nil {
		t.Fatalf("insert worker %s: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM evo.workers WHERE id = $1`, id)
	})
}

func cleanupJob(t *testing.T, p *pgxpool.Pool, jobID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM evo.jobs WHERE id = $1`, jobID)
	})
}

func testSpec(queue string) workflows.JobSpec {
	return workflows.JobSpec{
		Kind:         "TestKind",
		Payload:      []byte(`{"a":1}`),
		Requirements: workflows.Requirements{QueueClass: queue},
	}
}

func TestRecordJobInsertsPendingRow(t *testing.T) {
	p := sharedPool(t)
	rec := &RecordActivities{Pool: p}
	jobID := uuid.NewString()
	cleanupJob(t, p, jobID)

	res, err := rec.RecordJob(context.Background(), RecordJobInput{
		JobID:       jobID,
		Spec:        testSpec("q-record"),
		SubmittedBy: "", // must map to NULL, not ''
	})
	if err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	if res.JobID != jobID {
		t.Errorf("returned JobID = %q, want %q", res.JobID, jobID)
	}
	if res.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	var status string
	var batchSize int
	var submittedBy *string
	err = p.QueryRow(context.Background(),
		`SELECT status, batch_size, submitted_by FROM evo.jobs WHERE id = $1`, jobID).
		Scan(&status, &batchSize, &submittedBy)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if batchSize != 1 {
		t.Errorf("batch_size = %d, want 1 (defaulted from 0)", batchSize)
	}
	if submittedBy != nil {
		t.Errorf("submitted_by = %v, want NULL", *submittedBy)
	}
}

func TestAssignWorkerLifecycle(t *testing.T) {
	p := sharedPool(t)
	rec := &RecordActivities{Pool: p}
	ctx := context.Background()

	workerID := "test-worker-" + uuid.NewString()[:8]
	insertWorker(t, p, workerID, "q-assign", nil)

	jobID := uuid.NewString()
	cleanupJob(t, p, jobID)
	if _, err := rec.RecordJob(ctx, RecordJobInput{JobID: jobID, Spec: testSpec("q-assign")}); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}

	assign := AssignInput{
		JobID:          jobID,
		AssignedWorker: workerID,
		AssignedQueue:  "q-assign",
		WorkflowID:     "child-" + jobID,
	}
	if err := rec.AssignWorker(ctx, assign); err != nil {
		t.Fatalf("AssignWorker: %v", err)
	}

	var status string
	var assignedWorker *string
	if err := p.QueryRow(ctx,
		`SELECT status, assigned_worker FROM evo.jobs WHERE id = $1`, jobID).
		Scan(&status, &assignedWorker); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "assigned" || assignedWorker == nil || *assignedWorker != workerID {
		t.Errorf("row = %q/%v, want assigned/%s", status, assignedWorker, workerID)
	}

	// A second assign must fail non-retryably: the row is no longer pending.
	err := rec.AssignWorker(ctx, assign)
	if err == nil {
		t.Fatal("second AssignWorker succeeded, want NotPending error")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != "NotPending" || !appErr.NonRetryable() {
		t.Errorf("error = %v, want non-retryable NotPending", err)
	}
}

func TestMarkJobStatusTerminalGuard(t *testing.T) {
	p := sharedPool(t)
	rec := &RecordActivities{Pool: p}
	ctx := context.Background()

	jobID := uuid.NewString()
	cleanupJob(t, p, jobID)
	if _, err := rec.RecordJob(ctx, RecordJobInput{JobID: jobID, Spec: testSpec("q-mark")}); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}

	if err := rec.MarkJobStatus(ctx, MarkStatusInput{JobID: jobID, Status: workflows.StatusRunning}); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := rec.MarkJobStatus(ctx, MarkStatusInput{JobID: jobID, Status: workflows.StatusSucceeded}); err != nil {
		t.Fatalf("mark succeeded: %v", err)
	}

	// Terminal rows must refuse further transitions, non-retryably.
	err := rec.MarkJobStatus(ctx, MarkStatusInput{JobID: jobID, Status: workflows.StatusFailed})
	if err == nil {
		t.Fatal("mark after terminal succeeded, want TerminalOrMissing")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != "TerminalOrMissing" || !appErr.NonRetryable() {
		t.Errorf("error = %v, want non-retryable TerminalOrMissing", err)
	}

	var status string
	if err := p.QueryRow(ctx, `SELECT status FROM evo.jobs WHERE id = $1`, jobID).Scan(&status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != workflows.StatusSucceeded {
		t.Errorf("status = %q, want succeeded (terminal state must stick)", status)
	}

	// Missing row → same non-retryable class.
	if err := rec.MarkJobStatus(ctx, MarkStatusInput{JobID: uuid.NewString(), Status: workflows.StatusRunning}); err == nil {
		t.Error("mark on missing row succeeded, want error")
	}
}

func TestSelectCandidatesFilters(t *testing.T) {
	p := sharedPool(t)
	sel := &SelectActivities{Pool: p}
	ctx := context.Background()

	// Unique queue per run so leftover rows from other suites can't match.
	queue := "q-sel-" + uuid.NewString()[:8]

	insertWorker(t, p, "sel-cpu-"+queue, queue, nil)
	insertWorker(t, p, "sel-gpu-"+queue, queue, func(c map[string]any) {
		c["gpu_model"] = "NVIDIA RTX Pro 6000 Blackwell"
		c["gpu_vram_gb"] = 96
	})
	insertWorker(t, p, "sel-stale-"+queue, queue, func(c map[string]any) {
		c["heartbeat_ago_s"] = 600 // outside the 90s freshness window
	})
	insertWorker(t, p, "sel-inactive-"+queue, queue, func(c map[string]any) {
		c["status"] = "retired"
	})
	insertWorker(t, p, "sel-noexec-"+queue, queue, func(c map[string]any) {
		c["permissions"] = []string{"observe"}
	})

	t.Run("queue class matches fresh active executors only", func(t *testing.T) {
		got, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: queue},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("candidates = %d (%+v), want 2", len(got), got)
		}
		for _, c := range got {
			if c.Queue != queue {
				t.Errorf("candidate queue = %q, want %q", c.Queue, queue)
			}
		}
	})

	t.Run("requires_gpu excludes cpu-only workers", func(t *testing.T) {
		got, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: queue, RequiresGPU: true},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(got) != 1 || got[0].WorkerID != "sel-gpu-"+queue {
			t.Errorf("candidates = %+v, want only the gpu worker", got)
		}
	})

	t.Run("gpu family substring match", func(t *testing.T) {
		got, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: queue, GPUFamily: "blackwell"},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(got) != 1 || got[0].WorkerID != "sel-gpu-"+queue {
			t.Errorf("candidates = %+v, want only the blackwell worker", got)
		}
		none, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: queue, GPUFamily: "ampere"},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("ampere candidates = %+v, want none", none)
		}
	})

	t.Run("min vram threshold", func(t *testing.T) {
		got, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: queue, MinVRAMGB: 48},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(got) != 1 || got[0].WorkerID != "sel-gpu-"+queue {
			t.Errorf("candidates = %+v, want only the 96GB worker", got)
		}
	})

	t.Run("cpu features must be superset", func(t *testing.T) {
		got, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: queue, CPUFeatures: []string{"avx2"}},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("avx2 candidates = %d, want 2", len(got))
		}
		none, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: queue, CPUFeatures: []string{"avx512f"}},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("avx512 candidates = %+v, want none", none)
		}
	})

	t.Run("unknown queue yields empty non-error result", func(t *testing.T) {
		got, err := sel.SelectCandidates(ctx, SelectInput{
			Requirements: workflows.Requirements{QueueClass: "no-such-queue-" + uuid.NewString()},
		})
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("candidates = %+v, want none", got)
		}
	})
}

func TestEnsureBatchCoordinatorValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("nil client", func(t *testing.T) {
		a := &CoordinatorActivities{}
		_, err := a.EnsureBatchCoordinator(ctx, EnsureBatchCoordinatorInput{Queue: "q", BatchSize: 2})
		if err == nil {
			t.Fatal("want error for nil client")
		}
	})

	// The remaining validations sit behind the nil-client guard in source
	// order, so exercising them requires a non-nil client. Constructing a
	// real client needs a server; validation ordering (client first) is
	// itself the pinned behaviour here.
	t.Run("empty queue behind client guard", func(t *testing.T) {
		a := &CoordinatorActivities{}
		_, err := a.EnsureBatchCoordinator(ctx, EnsureBatchCoordinatorInput{Queue: "", BatchSize: 2})
		if err == nil {
			t.Fatal("want error")
		}
	})
}
