package registry

import (
	"context"
	"fmt"
)

// Dependency is an external prerequisite of a capability. Its name and kind
// are safe to publish. Check may be supplied by Go capabilities; otherwise
// the registry's dependency resolver handles it. Errors must omit secrets.
// Dependencies are checked at registration and on the registry's readiness
// refresh, and an unmet dependency removes the capability from tool lists.
type Dependency struct {
	Kind  string                      `json:"kind"`
	Name  string                      `json:"name"`
	Check func(context.Context) error `json:"-"`
}

// DependencyRef is the public, serializable part of a dependency.
type DependencyRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// APIKeyDependency requires a key from a setting or environment. The probe
// returns only presence, so no secret bytes enter the registry.
func APIKeyDependency(name string, configured func() bool) Dependency {
	return Dependency{Kind: "api_key", Name: name, Check: func(context.Context) error {
		if configured != nil && configured() {
			return nil
		}
		return fmt.Errorf("API key %s is not configured", name)
	}}
}

// ResourceDependency requires a named host resource, such as a worker image.
// The probe should inspect availability without starting or changing it.
func ResourceDependency(kind, name string, probe func(context.Context, string) error) Dependency {
	return Dependency{Kind: kind, Name: name, Check: func(ctx context.Context) error {
		if probe == nil {
			return fmt.Errorf("%s %s has no availability probe", kind, name)
		}
		if err := probe(ctx, name); err != nil {
			return fmt.Errorf("%s %s: %w", kind, name, err)
		}
		return nil
	}}
}

func hasCheck(c Capability) bool { return c.Check != nil || len(c.Dependencies) > 0 }

func (r *Registry) checkCapability(ctx context.Context, c Capability) error {
	for _, dependency := range c.Dependencies {
		check := dependency.Check
		if check == nil && r.cfg.dependencyResolver != nil {
			dep := dependency
			check = func(ctx context.Context) error { return r.cfg.dependencyResolver(ctx, dep) }
		}
		if check == nil {
			return fmt.Errorf("%s %s has no availability resolver", dependency.Kind, dependency.Name)
		}
		if err := check(ctx); err != nil {
			return err
		}
	}
	if c.Check != nil {
		return c.Check(ctx)
	}
	return nil
}
