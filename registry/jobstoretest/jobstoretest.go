// Package jobstoretest holds the conformance suite every registry.SharedJobs
// implementation must pass, and Memory, an in-process implementation for
// tests of hosts that run several registries in one process.
package jobstoretest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
)

// Memory returns a SharedJobs held in memory. Registries in one process that
// share it behave as instances sharing a database.
func Memory() registry.SharedJobs {
	return &memory{jobs: map[string]*memJob{}}
}

type memJob struct {
	rec    registry.JobRecord
	cancel bool
}

type memory struct {
	mu   sync.Mutex
	jobs map[string]*memJob
}

func (m *memory) Put(_ context.Context, rec registry.JobRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.jobs[rec.Status.ID]
	if !ok {
		m.jobs[rec.Status.ID] = &memJob{rec: rec}
		return nil
	}
	if cur.rec.Status.State.Terminal() || cur.rec.Status.Seq > rec.Status.Seq {
		return nil
	}
	cur.rec = rec
	return nil
}

func (m *memory) Get(_ context.Context, id string) (registry.JobRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return registry.JobRecord{}, false, nil
	}
	return j.rec, true, nil
}

func (m *memory) List(_ context.Context, keep time.Duration) ([]registry.JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-keep)
	out := []registry.JobRecord{}
	for _, j := range m.jobs {
		st := j.rec.Status
		if keep > 0 && st.State.Terminal() && st.Ended.Before(cutoff) {
			continue
		}
		out = append(out, j.rec)
	}
	return out, nil
}

func (m *memory) Claim(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || j.rec.Status.State != registry.JobQueued {
		return false, nil
	}
	j.rec.Status.State = registry.JobRunning
	j.rec.Status.Seq++
	return true, nil
}

func (m *memory) RequestCancel(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.cancel = true
	}
	return nil
}

func (m *memory) CancelRequested(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return ok && j.cancel, nil
}

// Fresh returns an empty store; shared stores return a handle on a fresh
// table each call. A second call with the same handle opens the same table.
type Fresh func(t *testing.T) (open func() registry.SharedJobs)

func record(id string, state registry.JobState, seq int64) registry.JobRecord {
	now := time.Now()
	st := registry.JobStatus{ID: id, Capability: "demo.build@1", State: state, Seq: seq, Started: now, Heartbeat: now,
		Caller: registry.Caller{ID: "user:ann", Kind: "user", Session: "s1"}}
	if state.Terminal() {
		st.Ended = now
	}
	return registry.JobRecord{Status: st, Long: true, Tags: []string{"build"}}
}

// Run checks the SharedJobs semantics the registry relies on.
func Run(t *testing.T, fresh Fresh) {
	ctx := context.Background()
	t.Run("put get and seq order", func(t *testing.T) {
		open := fresh(t)
		a, b := open(), open()
		if err := a.Put(ctx, record("j1", registry.JobQueued, 1)); err != nil {
			t.Fatal(err)
		}
		rec, ok, err := b.Get(ctx, "j1")
		if err != nil || !ok || rec.Status.State != registry.JobQueued || rec.Status.Caller.ID != "user:ann" || !rec.Long || rec.Tags[0] != "build" {
			t.Fatal(rec, ok, err)
		}
		must(t, a.Put(ctx, record("j1", registry.JobRunning, 3)))
		must(t, b.Put(ctx, record("j1", registry.JobRunning, 2))) // older: ignored
		if rec, _, _ := a.Get(ctx, "j1"); rec.Status.Seq != 3 {
			t.Fatal("an older record replaced a newer one", rec.Status.Seq)
		}
		if _, ok, err := a.Get(ctx, "missing"); ok || err != nil {
			t.Fatal(ok, err)
		}
	})
	t.Run("terminal is final", func(t *testing.T) {
		open := fresh(t)
		s := open()
		done := record("j2", registry.JobSucceeded, 4)
		done.Result = []byte(`{"ok":true}`)
		must(t, s.Put(ctx, done))
		must(t, s.Put(ctx, record("j2", registry.JobRunning, 9)))
		rec, _, _ := s.Get(ctx, "j2")
		if rec.Status.State != registry.JobSucceeded || string(rec.Result) != `{"ok":true}` {
			t.Fatal(rec)
		}
	})
	t.Run("claim once", func(t *testing.T) {
		open := fresh(t)
		s := open()
		must(t, s.Put(ctx, record("j3", registry.JobQueued, 1)))
		wins := make(chan bool, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				won, err := open().Claim(ctx, "j3")
				if err != nil {
					t.Error(err)
				}
				wins <- won
			}()
		}
		wg.Wait()
		close(wins)
		n := 0
		for w := range wins {
			if w {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d claims won, want 1", n)
		}
		if won, _ := s.Claim(ctx, "missing"); won {
			t.Fatal("claimed a missing job")
		}
	})
	t.Run("cancel request", func(t *testing.T) {
		open := fresh(t)
		a, b := open(), open()
		must(t, a.Put(ctx, record("j4", registry.JobRunning, 2)))
		if asked, _ := a.CancelRequested(ctx, "j4"); asked {
			t.Fatal("cancel requested before asking")
		}
		must(t, b.RequestCancel(ctx, "j4"))
		if asked, err := a.CancelRequested(ctx, "j4"); !asked || err != nil {
			t.Fatal(asked, err)
		}
	})
	t.Run("list keeps unfinished and recent", func(t *testing.T) {
		open := fresh(t)
		s := open()
		must(t, s.Put(ctx, record("live", registry.JobRunning, 2)))
		old := record("old", registry.JobSucceeded, 3)
		old.Status.Ended = time.Now().Add(-2 * time.Hour)
		must(t, s.Put(ctx, old))
		must(t, s.Put(ctx, record("recent", registry.JobFailed, 3)))
		recs, err := s.List(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, r := range recs {
			ids[r.Status.ID] = true
		}
		if !ids["live"] || !ids["recent"] || ids["old"] {
			t.Fatal(ids)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
