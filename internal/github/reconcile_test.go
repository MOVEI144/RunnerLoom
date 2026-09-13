package github

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/actions/scaleset"
)

type recordingJIT struct {
	name    string
	id      int
	set     int
	removed []int64
}

func (r *recordingJIT) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return nil, nil
}
func (r *recordingJIT) RemoveRunner(_ context.Context, id int64) error {
	r.removed = append(r.removed, id)
	return nil
}
func (r *recordingJIT) GenerateJitRunnerConfig(_ context.Context, q *scaleset.RunnerScaleSetJitRunnerSetting, set int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	name := r.name
	if name == "" {
		name = q.Name
	}
	sid := r.set
	if sid == 0 {
		sid = set
	}
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: r.id, Name: name, RunnerScaleSetID: sid}, EncodedJITConfig: "TEST_JIT_ONLY"}, nil
}

func reconcileReady(t *testing.T) (*core.Store, core.Pool) {
	t.Helper()
	s, e := core.OpenStore(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := core.Example()
	p, e := s.Plan(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	o := core.Observation{Node: c.Nodes[0].Name, BootID: "boot-a", Sequence: 1, Ready: true, Isolation: true, Ceiling: c.Nodes[0].LocalCeiling, FreeDiskGiB: 1000, Digests: []string{c.Images[0].Digest}}
	if _, e = s.Sync(context.Background(), o.Node, o); e != nil {
		t.Fatal(e)
	}
	if e = s.RefreshDemand(context.Background(), c.Pools[0].Name, 1); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Allocate(context.Background(), core.ID(), c.Pools[0].Name); e != nil {
		t.Fatal(e)
	}
	return s, c.Pools[0]
}

func TestReconcileRemovesMismatchedJITRunner(t *testing.T) {
	s, p := reconcileReady(t)
	api := &recordingJIT{name: "wrong-runner", id: 99, set: 1}
	e := Reconcile(context.Background(), s, p, 1, api)
	if e == nil {
		t.Fatal("mismatched JIT accepted")
	}
	if len(api.removed) != 1 || api.removed[0] != 99 {
		t.Fatalf("mismatched JIT runner not removed: %v", api.removed)
	}
}

func TestReconcileRemovesJITWhenSetFails(t *testing.T) {
	s, p := reconcileReady(t)
	api := &emptyJIT{recordingJIT: recordingJIT{id: 88, set: 1}}
	if e := Reconcile(context.Background(), s, p, 1, api); e == nil {
		t.Fatal("empty JIT accepted")
	}
	if len(api.removed) != 1 || api.removed[0] != 88 {
		t.Fatalf("failed SetJIT did not remove runner: %v", api.removed)
	}
}

type emptyJIT struct{ recordingJIT }

func (e *emptyJIT) GenerateJitRunnerConfig(ctx context.Context, q *scaleset.RunnerScaleSetJitRunnerSetting, set int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	jit, err := e.recordingJIT.GenerateJitRunnerConfig(ctx, q, set)
	if err != nil {
		return jit, err
	}
	jit.EncodedJITConfig = ""
	return jit, nil
}
