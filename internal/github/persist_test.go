package github

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/actions/scaleset"
)

func persistStore(t *testing.T) *core.Store {
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
	return s
}

func TestPersistThenAcknowledgeRequiresCompleteAcquire(t *testing.T) {
	s := persistStore(t)
	p := core.Example().Pools[0]
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:  7,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAvailableMessages: []*scaleset.JobAvailable{
			{RunnerRequestID: 11},
			{RunnerRequestID: 12},
		},
	}
	acked := false
	e := PersistThenAcknowledge(context.Background(), s, "sess", p, msg, func(context.Context, int) error {
		acked = true
		return nil
	}, func(context.Context, []int64) ([]int64, error) {
		return []int64{11}, nil
	})
	if e == nil || acked {
		t.Fatal("partial acquire was acknowledged", e)
	}
	d, e := s.Demands(context.Background())
	if e != nil || len(d) != 1 || !d[0].Blocked {
		t.Fatal("incomplete acquire did not fence demand", d, e)
	}
}

func TestPersistThenAcknowledgeClearsBarrierAfterAck(t *testing.T) {
	s := persistStore(t)
	p := core.Example().Pools[0]
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:  8,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAvailableMessages: []*scaleset.JobAvailable{
			{RunnerRequestID: 21},
		},
	}
	e := PersistThenAcknowledge(context.Background(), s, "sess", p, msg, func(context.Context, int) error { return nil }, func(_ context.Context, ids []int64) ([]int64, error) {
		return ids, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	d, e := s.Demands(context.Background())
	if e != nil || len(d) != 1 || d[0].Blocked {
		t.Fatal("successful acquire left demand fenced", d, e)
	}
}

func TestPersistThenAcknowledgeRetryClearsBarrier(t *testing.T) {
	s := persistStore(t)
	p := core.Example().Pools[0]
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:            10,
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAvailableMessages: []*scaleset.JobAvailable{{RunnerRequestID: 41}},
	}
	e := PersistThenAcknowledge(context.Background(), s, "sess", p, msg, func(context.Context, int) error { return nil }, func(context.Context, []int64) ([]int64, error) {
		return nil, errors.New("temporary")
	})
	if e == nil {
		t.Fatal("expected acquire failure")
	}
	e = PersistThenAcknowledge(context.Background(), s, "sess", p, msg, func(context.Context, int) error { return nil }, func(_ context.Context, ids []int64) ([]int64, error) {
		return ids, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	d, _ := s.Demands(context.Background())
	if len(d) != 1 || d[0].Blocked {
		t.Fatal("retry after acquire did not clear barrier", d)
	}
}

func TestPersistThenAcknowledgeFencesAcquireError(t *testing.T) {
	s := persistStore(t)
	p := core.Example().Pools[0]
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:            9,
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2},
		JobAvailableMessages: []*scaleset.JobAvailable{{RunnerRequestID: 31}},
	}
	e := PersistThenAcknowledge(context.Background(), s, "sess", p, msg, func(context.Context, int) error {
		t.Fatal("ack after acquire error")
		return nil
	}, func(context.Context, []int64) ([]int64, error) {
		return nil, errors.New("github unavailable")
	})
	if e == nil {
		t.Fatal("acquire error ignored")
	}
	d, _ := s.Demands(context.Background())
	if len(d) != 1 || !d[0].Blocked {
		t.Fatal("acquire error did not fence demand", d)
	}
}
