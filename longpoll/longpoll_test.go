package longpoll_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/longpoll"
	"github.com/craggy-io/capability-services/registry"
)

// log is a Source over a slice; positions are indexes.
type log struct {
	mu      sync.Mutex
	items   []string
	changed chan struct{}
	waits   int
}

func newLog() *log { return &log{changed: make(chan struct{})} }

func (l *log) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, s)
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *log) Changes(_ context.Context, after string, limit int) ([]string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	from := 0
	if after != "" {
		from, _ = strconv.Atoi(after)
	}
	to := min(len(l.items), from+limit)
	if from >= to {
		return nil, after, nil
	}
	return append([]string(nil), l.items[from:to]...), strconv.Itoa(to), nil
}

func (l *log) Wait(ctx context.Context, _ string) error {
	l.mu.Lock()
	ch := l.changed
	l.waits++
	l.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReturnsAtOnceWhenThereAreChanges(t *testing.T) {
	l := newLog()
	l.add("a")
	l.add("b")
	out, err := longpoll.Read[string](context.Background(), l, longpoll.In{Wait: 25})
	if err != nil || len(out.Changes) != 2 || out.Next != "2" || l.waits != 0 {
		t.Fatal(out, err, l.waits)
	}
	out, _ = longpoll.Read[string](context.Background(), l, longpoll.In{After: "1", Limit: 5})
	if len(out.Changes) != 1 || out.Changes[0] != "b" {
		t.Fatal(out)
	}
}

func TestWaitsForTheNextChange(t *testing.T) {
	l := newLog()
	go func() {
		time.Sleep(50 * time.Millisecond)
		l.add("late")
	}()
	start := time.Now()
	out, err := longpoll.Read[string](context.Background(), l, longpoll.In{Wait: 5})
	if err != nil || len(out.Changes) != 1 || out.Changes[0] != "late" {
		t.Fatal(out, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("waited past the change")
	}
}

func TestTimesOutWithNoChangesAndTheSamePosition(t *testing.T) {
	l := newLog()
	l.add("a")
	out, err := longpoll.Read[string](context.Background(), l, longpoll.In{After: "1", Wait: 0.05})
	if err != nil || len(out.Changes) != 0 || out.Next != "1" || out.Changes == nil {
		t.Fatal(out, err)
	}
}

func TestLimitsAndWaitAreBounded(t *testing.T) {
	l := newLog()
	for i := 0; i < longpoll.MaxLimit+5; i++ {
		l.add(strconv.Itoa(i))
	}
	out, _ := longpoll.Read[string](context.Background(), l, longpoll.In{Limit: 1 << 20})
	if len(out.Changes) != longpoll.MaxLimit {
		t.Fatal(len(out.Changes))
	}
	if _, err := longpoll.Read[string](context.Background(), l, longpoll.In{Wait: -1}); registry.CodeOf(err) != registry.Invalid {
		t.Fatal(err)
	}
}

func TestCancelledCallerStopsWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := longpoll.Read[string](ctx, newLog(), longpoll.In{Wait: 25}); registry.CodeOf(err) != registry.Cancelled {
		t.Fatal(err)
	}
}

func TestServedAsACapability(t *testing.T) {
	l := newLog()
	l.add("x")
	r := registry.New()
	t.Cleanup(r.Close)
	if err := r.Add(longpoll.Capability[string]("demo.changes", "Changes", l)); err != nil {
		t.Fatal(err)
	}
	raw, err := r.Invoke(context.Background(), "demo.changes", json.RawMessage(`{"after":"","wait":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var out longpoll.Out[string]
	if err := json.Unmarshal(raw, &out); err != nil || out.Next != "1" || out.Changes[0] != "x" {
		t.Fatal(string(raw), err)
	}
}
