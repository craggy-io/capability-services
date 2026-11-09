# Long-poll

[Project](../README.md) · [Documentation index](README.md) · [ADR 0001](decisions/0001-shared-jobs-and-dispatch.md)

## Purpose

Serve an ordered change log as an ordinary capability that waits: a client asks for the
changes after the position it holds, gets them at once, or, when there are none, waits up
to 25 seconds for the next one. It works through proxies that buffer responses (Firebase
Hosting, whose rewrite also times out at about 60 s) and across instances, when the source
reads shared storage.

## Public interfaces

```go
type Source[T any] interface {
	Changes(ctx context.Context, after string, limit int) (changes []T, next string, err error)
	Wait(ctx context.Context, after string) error
}

func Read[T any](ctx context.Context, src Source[T], in In) (Out[T], error)
func Capability[T any](name, description string, src Source[T], opts ...registry.Option) registry.Capability
```

- `In{After, Limit, Wait}`: the position read up to (`""` is the start), at most `Limit`
  changes (default 100, at most 1000), and how many seconds to wait when there are none
  (0 returns at once; capped at `MaxWait`, 25 s). A negative wait is `invalid`.
- `Out{Changes, Next}`: the changes, oldest first (never null), and the position to pass
  next. With no changes, `Next` is `After`.
- `Wait` may return spuriously; `Read` re-reads and waits again until the deadline. The
  caller's context ending is `cancelled`.

Positions are opaque strings chosen by the source: a journal sequence, a cursor.

## Example

From [longpoll_test.go](../longpoll/longpoll_test.go):

```go
r.Add(longpoll.Capability[string]("demo.changes", "Changes", source))
raw, _ := r.Invoke(ctx, "demo.changes", json.RawMessage(`{"after":"","wait":25}`))
// {"changes":["x"],"next":"1"}
```

A client loops: call with `after` set to the last `next`, process the changes, repeat.
