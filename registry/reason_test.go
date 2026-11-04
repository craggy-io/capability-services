package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/registry"
)

func TestReasonRoundTripThroughWrapping(t *testing.T) {
	cause := errors.New("pool has 4/4 emulators")
	err := registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, cause)
	if !errors.Is(err, cause) {
		t.Fatal("WrapReason lost its cause")
	}
	if err.Error() != "unavailable: pool has 4/4 emulators" {
		t.Fatalf("Error() = %q: the reason must not change the text", err.Error())
	}
	outer := fmt.Errorf("acquire device: %w", err)
	if registry.CodeOf(outer) != registry.Unavailable || registry.ReasonOf(outer) != registry.ReasonNoCapacity {
		t.Fatalf("code/reason through %%w = %s/%q", registry.CodeOf(outer), registry.ReasonOf(outer))
	}
	info := registry.Info(outer)
	if info.Code != registry.Unavailable || info.Reason != registry.ReasonNoCapacity || info.Message != "pool has 4/4 emulators" {
		t.Fatalf("Info = %+v", info)
	}
	b, _ := json.Marshal(info)
	if string(b) != `{"code":"unavailable","reason":"no_capacity","message":"pool has 4/4 emulators","retryable":true}` {
		t.Fatalf("ErrorInfo JSON = %s", b)
	}
	// An error with no reason omits it; retryable is always spelled out.
	b, _ = json.Marshal(registry.Info(registry.Errorf(registry.Denied, "no")))
	if string(b) != `{"code":"denied","message":"no","retryable":false}` {
		t.Fatalf("reason-less ErrorInfo JSON = %s", b)
	}

	// WrapCode and Codes.Wrap pass a reasoned error through untouched.
	if w := registry.WrapCode(registry.Internal, err); registry.ReasonOf(w) != registry.ReasonNoCapacity || registry.CodeOf(w) != registry.Unavailable {
		t.Fatalf("WrapCode changed a reasoned error: %v", w)
	}
	// WrapReason reclassifies deliberately, keeping the inner message.
	re := registry.WrapReason(registry.Unavailable, registry.ReasonBackendDown, registry.Errorf(registry.Internal, "adb: connection refused"))
	if registry.CodeOf(re) != registry.Unavailable || registry.ReasonOf(re) != registry.ReasonBackendDown || registry.Info(re).Message != "adb: connection refused" {
		t.Fatalf("WrapReason over a coded error = %+v", registry.Info(re))
	}

	if registry.WrapReason(registry.Unavailable, registry.ReasonBackendDown, nil) != nil {
		t.Fatal("WrapReason(nil) != nil")
	}
	if registry.ReasonOf(nil) != "" || registry.ReasonOf(errors.New("plain")) != "" || registry.ReasonOf(registry.Errorf(registry.Invalid, "x")) != "" {
		t.Fatal("ReasonOf of a reason-less error is not empty")
	}
	rf := registry.ReasonErrorf(registry.Unavailable, registry.ReasonNoCapacity, "pool %s full", "android")
	if registry.ReasonOf(rf) != registry.ReasonNoCapacity || rf.Error() != "unavailable: pool android full" {
		t.Fatalf("ReasonErrorf = %v (%q)", rf, registry.ReasonOf(rf))
	}
}

func TestValidReason(t *testing.T) {
	for _, ok := range []string{"", "no_capacity", "backend_down", "not_ready", "x", "adb2_lost"} {
		if !registry.ValidReason(ok) {
			t.Errorf("ValidReason(%q) = false", ok)
		}
	}
	for _, bad := range []string{"No_capacity", "no capacity", "no-capacity", "_x", "x_", "a__b", "2fast", "pool is full"} {
		if registry.ValidReason(bad) {
			t.Errorf("ValidReason(%q) = true", bad)
		}
	}
	for name, f := range map[string]func(){
		"WrapReason":   func() { _ = registry.WrapReason(registry.Unavailable, "pool is full", errors.New("x")) },
		"ReasonErrorf": func() { _ = registry.ReasonErrorf(registry.Unavailable, "Backend-Down", "x") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s accepted free-text reason", name)
				}
			}()
			f()
		}()
	}
}

func TestNotReadyCarriesReason(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	down := registry.Func("down.cap", "Down",
		func(ctx context.Context, in struct{}) (struct{}, error) { return in, nil },
		registry.WithCheck(func(ctx context.Context) error { return errors.New("no docker") }))
	mustAdd(t, r, down)
	_, err := r.Invoke(context.Background(), "down.cap", nil)
	if registry.CodeOf(err) != registry.Unavailable || registry.ReasonOf(err) != registry.ReasonNotReady {
		t.Fatalf("not ready: %s/%q (%v)", registry.CodeOf(err), registry.ReasonOf(err), err)
	}
}

func TestJobFailurePreservesReason(t *testing.T) {
	r := registry.New()
	t.Cleanup(r.Close)
	full := func(ctx context.Context, in struct{}) (struct{}, error) {
		return in, registry.WrapReason(registry.Unavailable, registry.ReasonNoCapacity, errors.New("pool full"))
	}
	mustAdd(t, r,
		registry.Func("pool.sync", "Sync", full),
		registry.Func("pool.long", "Long", full, registry.AsLong(time.Second)),
		registry.Func("pool.cancel", "Cancel", func(ctx context.Context, in struct{}) (struct{}, error) {
			return in, registry.WrapReason(registry.Cancelled, registry.ReasonBackendDown, errors.New("backend went away"))
		}),
	)
	v := r.View(registry.All)
	ctx := context.Background()

	// Invoke is Start + Wait: the reason must survive the job round trip.
	if _, err := v.Invoke(ctx, "pool.sync", nil); registry.CodeOf(err) != registry.Unavailable || registry.ReasonOf(err) != registry.ReasonNoCapacity {
		t.Fatalf("Invoke: %s/%q (%v)", registry.CodeOf(err), registry.ReasonOf(err), err)
	}
	if _, err := v.Invoke(ctx, "pool.cancel", nil); registry.CodeOf(err) != registry.Cancelled || registry.ReasonOf(err) != registry.ReasonBackendDown {
		t.Fatalf("Invoke cancelled: %s/%q (%v)", registry.CodeOf(err), registry.ReasonOf(err), err)
	}

	job, err := v.Start(ctx, "pool.long", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = job.Wait(ctx)
	if registry.CodeOf(err) != registry.Unavailable || registry.ReasonOf(err) != registry.ReasonNoCapacity || registry.Info(err).Message != "pool full" {
		t.Fatalf("Wait: %+v", registry.Info(err))
	}
	st := job.Status()
	if st.State != registry.JobFailed || st.Error == nil || st.Error.Reason != registry.ReasonNoCapacity {
		t.Fatalf("status = %s %+v", st.State, st.Error)
	}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"error":{"code":"unavailable","reason":"no_capacity","message":"pool full","retryable":true}`) {
		t.Fatalf("status JSON lacks reason: %s", b)
	}
	var last registry.Event
	for ev := range job.Events(ctx) {
		last = ev
	}
	if last.Error == nil || last.Error.Reason != registry.ReasonNoCapacity {
		t.Fatalf("terminal event error = %+v", last.Error)
	}

}
