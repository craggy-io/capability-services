package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/tools"
)

func TestDependenciesHideAndRestoreTool(t *testing.T) {
	key := false
	containerReady := false
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	cap := registry.Func("demo.dependent", "Dependent", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		registry.WithDependencies(
			registry.APIKeyDependency("demo.api-key", func() bool { return key }),
			registry.ResourceDependency("container", "demo-worker", func(_ context.Context, name string) error {
				if name != "demo-worker" || !containerReady {
					return errors.New("container unavailable")
				}
				return nil
			}),
		))
	if err := r.Add(cap); err != nil {
		t.Fatal(err)
	}
	v := r.View(registry.All)
	assertHidden := func(reason string) {
		t.Helper()
		if len(tools.Defs(v)) != 0 {
			t.Fatal("unready tool was advertised")
		}
		d, ok := v.Get("demo.dependent")
		if !ok || d.Ready || !strings.Contains(d.CheckError, reason) {
			t.Fatalf("entry: %+v", d)
		}
		if _, err := v.Invoke(context.Background(), "demo.dependent", json.RawMessage(`{}`)); registry.CodeOf(err) != registry.Unavailable {
			t.Fatalf("invoke: %v", err)
		}
	}
	assertHidden("demo.api-key")
	key = true
	r.RefreshChecks(context.Background())
	assertHidden("demo-worker")
	containerReady = true
	r.RefreshChecks(context.Background())
	if defs := tools.Defs(v); len(defs) != 1 || defs[0].Name != "demo_dependent" {
		t.Fatalf("ready defs: %+v", defs)
	}
	d := v.Descriptors(false)[0]
	if !d.Ready || !d.HasCheck || len(d.Dependencies) != 2 || d.Dependencies[0].Kind != "api_key" || d.Dependencies[1].Name != "demo-worker" {
		t.Fatalf("descriptor: %+v", d)
	}
	b, err := json.Marshal(d)
	if err != nil || strings.Contains(string(b), "secret") || strings.Contains(string(b), `"Check":`) {
		t.Fatalf("descriptor leaked probe/key: %s %v", b, err)
	}
	containerReady = false
	r.RefreshChecks(context.Background())
	assertHidden("demo-worker")
}

func TestDependencyWithoutResolverStaysUnavailable(t *testing.T) {
	r := registry.New(registry.CheckEvery(0))
	defer r.Close()
	cap := registry.Func("demo.invalid", "Invalid", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		registry.WithDependencies(registry.Dependency{Kind: "container", Name: "worker"}))
	if err := r.Add(cap); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if e, _ := r.Get("demo.invalid"); e.Ready || !strings.Contains(e.CheckError, "no availability resolver") {
		t.Fatalf("entry: %+v", e)
	}
}

func TestDeclaredDependencyUsesRegistryResolver(t *testing.T) {
	ready := false
	r := registry.New(registry.CheckEvery(0), registry.WithDependencyResolver(func(_ context.Context, dep registry.Dependency) error {
		if dep.Kind != "container" || dep.Name != "worker" || !ready {
			return errors.New("worker unavailable")
		}
		return nil
	}))
	defer r.Close()
	cap := registry.Func("demo.worker", "Worker", func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		registry.WithDependencies(registry.Dependency{Kind: "container", Name: "worker"}))
	if err := r.Add(cap); err != nil {
		t.Fatal(err)
	}
	if e, _ := r.Get("demo.worker"); e.Ready {
		t.Fatal("unavailable worker advertised")
	}
	ready = true
	r.RefreshDependency(context.Background(), "container", "worker")
	if e, _ := r.Get("demo.worker"); !e.Ready {
		t.Fatalf("ready worker hidden: %+v", e)
	}
}
