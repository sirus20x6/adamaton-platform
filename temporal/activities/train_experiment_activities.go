package activities

// Training-experiment activities. Three concerns:
//
//   ExperimentMarkRunning    — DB: stamp status=running + workflow_id
//   DispatchTrainOnBlackwell — ssh+scp to NANO_BLACKWELL_HOST (reuses
//                              the trust + key already established by
//                              deepresearch/nano-research)
//   IngestExperimentMetrics  — parse metrics.json, bulk INSERT rows
//   ExperimentFinalize       — DB: stamp terminal status + val_bpb + finished_at
//
// Deps are injected via ExperimentDeps so the worker process owns the
// pool lifetime; activities are method-bound so RegisterActivity sees
// them as a single struct.
//
// SSH/SCP environment mirrors nano-research's pattern intentionally:
//
//	NANO_BLACKWELL_HOST     — ssh target; required
//	NANO_BLACKWELL_SSH_KEY  — private key path (default /home/sirus/.ssh/id_ed25519)
//
// The remote script is expected at /opt/adamaton/run_train.sh on
// blackwell. It receives the experiment_id + dataset_version_id + split
// + caller-supplied extra args as positional argv, runs whatever
// training entrypoint it wants, and writes /tmp/exp-<id>/metrics.json
// in the agreed schema (see ParseMetricsJSON for the shape).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
)

// ExperimentDeps holds the resources the experiment activities need.
// One instance lives in the worker process; methods are registered on
// *ExperimentDeps so Temporal picks them up by reflected name.
type ExperimentDeps struct {
	Pool *pgxpool.Pool
}

/* ── ExperimentMarkRunning ─────────────────────────────────────────────── */

type ExperimentMarkRunningInput struct {
	ExperimentID string `json:"experiment_id"`
	WorkflowID   string `json:"workflow_id"`
}

func (d *ExperimentDeps) ExperimentMarkRunning(ctx context.Context, in ExperimentMarkRunningInput) error {
	if d.Pool == nil {
		return errors.New("ExperimentMarkRunning: nil pool")
	}
	const sql = `
UPDATE platform.experiments
SET status = 'running',
    workflow_id = $2,
    started_at = NOW()
WHERE id = $1`
	_, err := d.Pool.Exec(ctx, sql, in.ExperimentID, in.WorkflowID)
	return err
}

/* ── DispatchTrainOnBlackwell ──────────────────────────────────────────── */

type DispatchTrainInput struct {
	ExperimentID     string   `json:"experiment_id"`
	DatasetVersionID string   `json:"dataset_version_id,omitempty"`
	Split            string   `json:"split,omitempty"`
	Cmd              []string `json:"cmd"`
	TimeoutSeconds   int      `json:"timeout_seconds,omitempty"`
}

type DispatchTrainOutput struct {
	ExitCode    int    `json:"exit_code"`
	StdoutTail  string `json:"stdout_tail"`
	StderrTail  string `json:"stderr_tail"`
	MetricsJSON string `json:"metrics_json"` // raw JSON content of metrics.json, empty if scp failed
}

// DispatchTrainOnBlackwell ssh's the cmd, then scp's
// /tmp/exp-<id>/metrics.json back into the activity output. We pull the
// metrics file content into the workflow payload (bounded by Temporal's
// 2MB limit — fine for a few thousand metric points; if we ever exceed
// that, switch to S3 staging) so a separate activity can ingest it
// transactionally without re-touching the GPU node.
func (d *ExperimentDeps) DispatchTrainOnBlackwell(ctx context.Context, in DispatchTrainInput) (DispatchTrainOutput, error) {
	host := os.Getenv("NANO_BLACKWELL_HOST")
	if host == "" {
		return DispatchTrainOutput{}, errors.New("DispatchTrainOnBlackwell: NANO_BLACKWELL_HOST is empty")
	}
	sshKey := os.Getenv("NANO_BLACKWELL_SSH_KEY")
	if sshKey == "" {
		sshKey = "/home/sirus/.ssh/id_ed25519"
	}
	if in.ExperimentID == "" {
		return DispatchTrainOutput{}, errors.New("DispatchTrainOnBlackwell: experiment_id required")
	}
	if len(in.Cmd) == 0 {
		return DispatchTrainOutput{}, errors.New("DispatchTrainOnBlackwell: cmd required")
	}

	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}

	sshOpts := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=30",
		"-i", sshKey,
	}

	// Build remote line: cd /tmp/exp-<id> && <cmd>. The remote
	// /opt/adamaton/run_train.sh wrapper is expected to mkdir -p that dir
	// before doing anything else.
	remoteDir := "/tmp/exp-" + in.ExperimentID
	mkdirThenCmd := "mkdir -p " + shellQuoteExp(remoteDir) + " && cd " + shellQuoteExp(remoteDir) + " && " + joinShellExp(in.Cmd)

	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sshArgs := append([]string{}, sshOpts...)
	sshArgs = append(sshArgs, host, mkdirThenCmd)
	cmd := exec.CommandContext(subCtx, "ssh", sshArgs...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	go heartbeatExp(ctx, subCtx, in.ExperimentID)

	stdout, runErr := cmd.Output()
	stderr := stderrBuf.Bytes()
	out := DispatchTrainOutput{
		StdoutTail: tailStringExp(string(stdout), 4096),
		StderrTail: tailStringExp(string(stderr), 4096),
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			out.ExitCode = exitErr.ExitCode()
			if len(stderr) == 0 {
				out.StderrTail = tailStringExp(string(exitErr.Stderr), 4096)
			}
			// Non-zero exit is a "training failed cleanly" signal —
			// surface to workflow as a normal return so finalize stamps
			// status=failed. We still try to fetch metrics.json (some
			// trainers emit partial metrics before crashing).
		} else {
			// Non-exit failure: timeout, ssh transport down.
			if errors.Is(subCtx.Err(), context.DeadlineExceeded) {
				return out, fmt.Errorf("DispatchTrainOnBlackwell: ssh timed out after %s: %w", timeout, runErr)
			}
			return out, fmt.Errorf("DispatchTrainOnBlackwell: ssh transport failed: %w (stderr=%s)", runErr, out.StderrTail)
		}
	}

	// SCP metrics.json back. Best-effort — if the trainer didn't write
	// one (early crash, no metrics emitter), we leave MetricsJSON empty
	// and let the workflow finalize with PointsIngested=0.
	metricsJSON, scpErr := d.fetchRemoteFile(ctx, host, sshOpts, remoteDir+"/metrics.json", 30*time.Second)
	if scpErr != nil {
		activity.GetLogger(ctx).Warn("metrics.json fetch failed; experiment will finalize without points",
			"experiment_id", in.ExperimentID, "error", scpErr.Error())
	} else {
		out.MetricsJSON = string(metricsJSON)
	}

	// Best-effort cleanup of the remote dir. Detached context so it runs
	// even if the parent ctx already cancelled.
	d.cleanupRemoteExp(host, sshOpts, remoteDir)

	return out, nil
}

func (d *ExperimentDeps) fetchRemoteFile(ctx context.Context, host string, sshOpts []string, remotePath string, timeout time.Duration) ([]byte, error) {
	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append([]string{}, sshOpts...)
	args = append(args, host, "cat "+shellQuoteExp(remotePath))
	cmd := exec.CommandContext(subCtx, "ssh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	// Bound at 1.5 MB so we stay well below Temporal's 2 MB payload limit
	// even with workflow envelope overhead.
	const maxBytes = 1_500_000
	if len(out) > maxBytes {
		return nil, fmt.Errorf("metrics.json too large: %d bytes (max %d)", len(out), maxBytes)
	}
	return out, nil
}

func (d *ExperimentDeps) cleanupRemoteExp(host string, sshOpts []string, remoteDir string) {
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	args := append([]string{}, sshOpts...)
	args = append(args, host, "rm -rf "+shellQuoteExp(remoteDir))
	_ = exec.CommandContext(cctx, "ssh", args...).Run()
}

/* ── IngestExperimentMetrics ───────────────────────────────────────────── */

type IngestMetricsInput struct {
	ExperimentID string `json:"experiment_id"`
	MetricsJSON  string `json:"metrics_json"`
}

type IngestMetricsOutput struct {
	PointsIngested int      `json:"points_ingested"`
	ValBPB         *float64 `json:"val_bpb,omitempty"` // last 'val_bpb' sample if present
}

// metricsFileShape mirrors what we expect the remote train script to
// emit. Each entry is one (step, key, value) sample. Trainers may emit
// the same key at multiple steps; we preserve every row.
type metricsFileShape struct {
	Points []struct {
		Step  int     `json:"step"`
		Key   string  `json:"key"`
		Value float64 `json:"value"`
	} `json:"points"`
}

func (d *ExperimentDeps) IngestExperimentMetrics(ctx context.Context, in IngestMetricsInput) (IngestMetricsOutput, error) {
	if d.Pool == nil {
		return IngestMetricsOutput{}, errors.New("IngestExperimentMetrics: nil pool")
	}
	if in.MetricsJSON == "" {
		return IngestMetricsOutput{}, nil
	}
	var parsed metricsFileShape
	if err := json.Unmarshal([]byte(in.MetricsJSON), &parsed); err != nil {
		return IngestMetricsOutput{}, fmt.Errorf("parse metrics.json: %w", err)
	}
	if len(parsed.Points) == 0 {
		return IngestMetricsOutput{}, nil
	}

	// Bulk insert via a single multi-row VALUES list. For typical run
	// sizes (~1k points) one statement is faster than a CopyFrom setup
	// and stays inside a single transaction without explicit BEGIN.
	args := make([]any, 0, 4*len(parsed.Points))
	placeholders := make([]string, 0, len(parsed.Points))
	var lastValBPB *float64
	for i, p := range parsed.Points {
		base := i*4 + 1
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $%d, $%d)", base, base+1, base+2, base+3))
		args = append(args, in.ExperimentID, p.Step, p.Key, p.Value)
		if p.Key == "val_bpb" {
			v := p.Value
			lastValBPB = &v
		}
	}
	sql := "INSERT INTO platform.experiment_metrics (experiment_id, step, key, value) VALUES " + strings.Join(placeholders, ", ")
	if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
		return IngestMetricsOutput{}, fmt.Errorf("insert metrics: %w", err)
	}
	return IngestMetricsOutput{PointsIngested: len(parsed.Points), ValBPB: lastValBPB}, nil
}

/* ── ExperimentFinalize ────────────────────────────────────────────────── */

type ExperimentFinalizeInput struct {
	ExperimentID string   `json:"experiment_id"`
	Status       string   `json:"status"` // succeeded | failed
	ValBPB       *float64 `json:"val_bpb,omitempty"`
	ExitCode     int      `json:"exit_code"`
	StderrTail   string   `json:"stderr_tail,omitempty"`
}

func (d *ExperimentDeps) ExperimentFinalize(ctx context.Context, in ExperimentFinalizeInput) error {
	if d.Pool == nil {
		return errors.New("ExperimentFinalize: nil pool")
	}
	// notes field captures the stderr_tail when failed, so operators
	// can see the immediate cause in the UI without opening Temporal.
	var notes *string
	if in.Status == "failed" && in.StderrTail != "" {
		s := fmt.Sprintf("exit_code=%d\n--- stderr tail ---\n%s", in.ExitCode, in.StderrTail)
		notes = &s
	}
	const sql = `
UPDATE platform.experiments
SET status      = $2,
    val_bpb     = COALESCE($3, val_bpb),
    notes       = COALESCE($4, notes),
    finished_at = NOW()
WHERE id = $1`
	_, err := d.Pool.Exec(ctx, sql, in.ExperimentID, in.Status, in.ValBPB, notes)
	return err
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func heartbeatExp(parentCtx, subCtx context.Context, label string) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-subCtx.Done():
			return
		case <-t.C:
			activity.RecordHeartbeat(parentCtx, label)
		}
	}
}

func tailStringExp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(truncated)..." + s[len(s)-n:]
}

func shellQuoteExp(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func joinShellExp(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuoteExp(a)
	}
	return strings.Join(parts, " ")
}

// Workflow-side dispatch uses the nil-pointer convention from
// go.temporal.io/sdk: the workflow declares `var a *ExperimentDeps` and
// calls `a.MethodName` so RegisterActivity's reflection-based name
// lookup matches a registered impl in the worker. No package-level
// aliases needed; the workflow imports ExperimentDeps directly.
