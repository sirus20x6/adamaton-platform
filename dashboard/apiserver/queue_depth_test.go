package apiserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.temporal.io/sdk/client"
)

// fakeDescriber returns canned per-queue descriptions (or errors).
type fakeDescriber struct {
	byQueue map[string]client.TaskQueueDescription
	errFor  map[string]error
	calls   []string
}

func (f *fakeDescriber) DescribeTaskQueueEnhanced(_ context.Context, opts client.DescribeTaskQueueEnhancedOptions) (client.TaskQueueDescription, error) {
	f.calls = append(f.calls, opts.TaskQueue)
	if err, ok := f.errFor[opts.TaskQueue]; ok {
		return client.TaskQueueDescription{}, err
	}
	return f.byQueue[opts.TaskQueue], nil
}

func descWithStats(wfDepth, actDepth int64, wfAge, actAge time.Duration) client.TaskQueueDescription {
	return client.TaskQueueDescription{
		VersionsInfo: map[string]client.TaskQueueVersionInfo{
			"": {
				TypesInfo: map[client.TaskQueueType]client.TaskQueueTypeInfo{
					client.TaskQueueTypeWorkflow: {Stats: &client.TaskQueueStats{
						ApproximateBacklogCount: wfDepth,
						ApproximateBacklogAge:   wfAge,
					}},
					client.TaskQueueTypeActivity: {Stats: &client.TaskQueueStats{
						ApproximateBacklogCount: actDepth,
						ApproximateBacklogAge:   actAge,
					}},
				},
			},
		},
	}
}

func TestQueueDepthPollerSetsGauges(t *testing.T) {
	fake := &fakeDescriber{
		byQueue: map[string]client.TaskQueueDescription{
			"skills": descWithStats(3, 42, 5*time.Second, 90*time.Second),
		},
	}
	p := newQueueDepthPoller(fake, []string{"skills"}, time.Minute, nil)
	p.pollOnce(context.Background())

	if got := testutil.ToFloat64(TemporalQueueDepth.WithLabelValues("skills", "workflow")); got != 3 {
		t.Errorf("workflow depth = %v, want 3", got)
	}
	if got := testutil.ToFloat64(TemporalQueueDepth.WithLabelValues("skills", "activity")); got != 42 {
		t.Errorf("activity depth = %v, want 42", got)
	}
	if got := testutil.ToFloat64(TemporalQueueDepthAgeSeconds.WithLabelValues("skills", "activity")); got != 90 {
		t.Errorf("activity backlog age = %v, want 90", got)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "skills" {
		t.Errorf("calls = %v, want [skills]", fake.calls)
	}
}

func TestQueueDepthPollerSumsAcrossVersions(t *testing.T) {
	desc := client.TaskQueueDescription{
		VersionsInfo: map[string]client.TaskQueueVersionInfo{
			"": {TypesInfo: map[client.TaskQueueType]client.TaskQueueTypeInfo{
				client.TaskQueueTypeActivity: {Stats: &client.TaskQueueStats{
					ApproximateBacklogCount: 7, ApproximateBacklogAge: 10 * time.Second,
				}},
			}},
			"build-2": {TypesInfo: map[client.TaskQueueType]client.TaskQueueTypeInfo{
				client.TaskQueueTypeActivity: {Stats: &client.TaskQueueStats{
					ApproximateBacklogCount: 5, ApproximateBacklogAge: 30 * time.Second,
				}},
			}},
		},
	}
	depth, age := sumQueueStats(desc, client.TaskQueueTypeActivity)
	if depth != 12 {
		t.Errorf("depth = %d, want 12", depth)
	}
	if age != 30*time.Second {
		t.Errorf("age = %v, want 30s", age)
	}
	// Missing stats (ReportStats unsupported / nil) contribute nothing.
	depth, age = sumQueueStats(client.TaskQueueDescription{}, client.TaskQueueTypeWorkflow)
	if depth != 0 || age != 0 {
		t.Errorf("empty desc = (%d, %v), want (0, 0)", depth, age)
	}
}

func TestQueueDepthPollerErrorKeepsLastValueAndCounts(t *testing.T) {
	fake := &fakeDescriber{
		byQueue: map[string]client.TaskQueueDescription{
			"dispatch": descWithStats(9, 0, 0, 0),
		},
	}
	p := newQueueDepthPoller(fake, []string{"dispatch"}, time.Minute, nil)
	p.pollOnce(context.Background())
	if got := testutil.ToFloat64(TemporalQueueDepth.WithLabelValues("dispatch", "workflow")); got != 9 {
		t.Fatalf("depth after first poll = %v, want 9", got)
	}

	errsBefore := testutil.ToFloat64(TemporalQueueDepthPollErrors.WithLabelValues("dispatch"))
	fake.errFor = map[string]error{"dispatch": errors.New("temporal unreachable")}
	p.pollOnce(context.Background())

	if got := testutil.ToFloat64(TemporalQueueDepth.WithLabelValues("dispatch", "workflow")); got != 9 {
		t.Errorf("depth after failed poll = %v, want last value 9", got)
	}
	if got := testutil.ToFloat64(TemporalQueueDepthPollErrors.WithLabelValues("dispatch")); got != errsBefore+1 {
		t.Errorf("poll errors = %v, want %v", got, errsBefore+1)
	}
}
