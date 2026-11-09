// Package longpoll serves an ordered change log as a capability that waits:
// a call returns the changes after a position at once, or, when there are
// none, waits up to MaxWait for one. It works through proxies that buffer
// responses (Firebase Hosting buffers, so Server-Sent Events do not stream
// through it) and across instances, when the Source reads shared storage.
// See docs/longpoll.md.
package longpoll

import (
	"context"
	"time"

	"github.com/craggy-io/capability-services/registry"
)

// MaxWait bounds one call's wait, under the roughly 60 s a buffering proxy
// allows a request.
const MaxWait = 25 * time.Second

// DefaultLimit and MaxLimit bound the changes one call returns.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// Source is an ordered change log. Positions are opaque strings; "" is the
// start.
type Source[T any] interface {
	// Changes returns up to limit changes after position after, oldest first,
	// and the position after the last one returned (after itself when none).
	Changes(ctx context.Context, after string, limit int) (changes []T, next string, err error)
	// Wait returns when a change after position after may exist, or with
	// ctx's error when ctx ends. A spurious return is allowed.
	Wait(ctx context.Context, after string) error
}

// In is a call's input.
type In struct {
	// After is the position the caller has read up to; "" reads from the start.
	After string `json:"after,omitempty"`
	// Limit bounds the changes returned (default 100, at most 1000).
	Limit int `json:"limit,omitempty"`
	// Wait is how many seconds to wait for a change when there is none yet
	// (0 returns at once; at most 25).
	Wait float64 `json:"wait,omitempty"`
}

// Out is a call's result: the changes, oldest first, and the position to pass
// as After next time.
type Out[T any] struct {
	Changes []T    `json:"changes"`
	Next    string `json:"next"`
}

// Read returns the changes after in.After, waiting up to in.Wait for one when
// there are none.
func Read[T any](ctx context.Context, src Source[T], in In) (Out[T], error) {
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if in.Wait < 0 {
		return Out[T]{}, registry.Errorf(registry.Invalid, "wait must not be negative")
	}
	wait := time.Duration(in.Wait * float64(time.Second))
	if wait > MaxWait {
		wait = MaxWait
	}
	deadline := time.Now().Add(wait)
	for {
		changes, next, err := src.Changes(ctx, in.After, limit)
		if err != nil {
			return Out[T]{}, err
		}
		if len(changes) > 0 || !time.Now().Before(deadline) {
			if changes == nil {
				changes = []T{}
			}
			return Out[T]{Changes: changes, Next: next}, nil
		}
		wctx, cancel := context.WithDeadline(ctx, deadline)
		err = src.Wait(wctx, in.After)
		cancel()
		if ctx.Err() != nil {
			return Out[T]{}, registry.WrapCode(registry.Cancelled, ctx.Err())
		}
		if err != nil && wctx.Err() == nil {
			return Out[T]{}, err
		}
	}
}

// Capability serves src as the capability name. Callers poll it with the Next
// they were last given.
func Capability[T any](name, description string, src Source[T], opts ...registry.Option) registry.Capability {
	return registry.Func(name, description, func(ctx context.Context, in In) (Out[T], error) {
		return Read(ctx, src, in)
	}, opts...)
}
