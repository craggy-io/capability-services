package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/craggy-io/capability-services/registry"
)

type greetIn struct {
	Name string `json:"name" jsonschema:"who to greet"`
}

type greetOut struct {
	Greeting string `json:"greeting"`
}

func greet() registry.Capability {
	return registry.Func("demo.greet", "Greet someone",
		func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "hello " + in.Name}, nil
		},
		registry.Describe("Return a greeting for name."),
		registry.WithEffects(registry.ReadOnly|registry.Idempotent),
		registry.WithTags("agent"),
		registry.WithExample("the usual call", greetIn{Name: "ada"}))
}

// Register a typed Go function once, then invoke it. Input is validated
// against the derived schema before the handler runs; the invalid message
// is written for the caller (often a model) to fix the call.
func Example() {
	r := registry.New()
	defer r.Close()
	if err := r.Add(greet()); err != nil {
		panic(err)
	}
	ctx := context.Background()

	out, err := r.Invoke(ctx, "demo.greet", json.RawMessage(`{"name":"ada"}`))
	fmt.Println(string(out), err)

	_, err = r.Invoke(ctx, "demo.greet", json.RawMessage(`{"nmae":"ada"}`))
	fmt.Println(registry.CodeOf(err))
	fmt.Println(registry.Info(err).Message)
	// Output:
	// {"greeting":"hello ada"} <nil>
	// invalid
	// demo.greet@1: invalid input:
	// - name: missing required field
	// - nmae: unknown field; accepted fields: name
	// Required fields: name.
	// Example input: {"name":"ada"}
}

// Versions are worked out from what is registered: the highest version is
// current, a bare name resolves to it, and name@N pins an older one.
func Example_versions() {
	r := registry.New()
	defer r.Close()
	v := func(n int) registry.Capability {
		return registry.Func("demo.version", "Report a version",
			func(ctx context.Context, _ struct{}) (int, error) { return n, nil },
			registry.WithVersion(n))
	}
	if err := r.Add(v(1), v(2)); err != nil {
		panic(err)
	}
	ctx := context.Background()
	bare, _ := r.Invoke(ctx, "demo.version", nil)
	pinned, _ := r.Invoke(ctx, "demo.version@1", nil)
	fmt.Println("bare:", string(bare), "pinned:", string(pinned))
	for _, e := range r.All() {
		fmt.Println(e.Capability.Ref(), "current:", e.Current)
	}

	r.Remove("demo.version@2") // retire a version
	e, _ := r.Get("demo.version")
	fmt.Println("now current:", e.Capability.Ref())
	// Output:
	// bare: 2 pinned: 1
	// demo.version@1 current: false
	// demo.version@2 current: true
	// now current: demo.version@1
}

// A capability can supersede others, including ones registered by someone
// else. Superseded entries leave List (so agents stop seeing them) but still
// answer when called by name; removing the replacement brings them back.
func Example_supersession() {
	r := registry.New()
	defer r.Close()
	legacy := registry.Func("jadx.decompile", "Decompile (legacy)",
		func(ctx context.Context, _ struct{}) (string, error) { return "jadx", nil })
	modern := registry.Func("apk.decompile", "Decompile",
		func(ctx context.Context, _ struct{}) (string, error) { return "apk", nil },
		registry.WithSupersedes("jadx.decompile"))
	if err := r.Add(legacy); err != nil {
		panic(err)
	}
	if err := r.Replace("apk", modern); err != nil { // load a runtime source
		panic(err)
	}
	names := func() (out []string) {
		for _, e := range r.List() {
			out = append(out, e.Name)
		}
		return out
	}
	fmt.Println("list:", names())
	old, _ := r.Get("jadx.decompile")
	fmt.Println("jadx superseded by:", old.SupersededBy)
	out, _ := r.Invoke(context.Background(), "jadx.decompile", nil)
	fmt.Println("jadx still answers:", string(out))

	if err := r.Replace("apk"); err != nil { // unload the source
		panic(err)
	}
	fmt.Println("after unload:", names())
	// Output:
	// list: [apk.decompile]
	// jadx superseded by: [apk.decompile]
	// jadx still answers: "jadx"
	// after unload: [jadx.decompile]
}

// Views are policy: filtering and enforcement are the same thing. A name
// outside the view is not_found, and a nested call made from a handler is
// clamped to the view the outer call arrived on, even when the handler
// reaches for the whole registry.
func Example_views() {
	r := registry.New()
	defer r.Close()
	wipe := registry.Func("admin.wipe", "Wipe everything",
		func(ctx context.Context, _ struct{}) (string, error) { return "wiped", nil },
		registry.WithTags("admin"), registry.WithEffects(registry.Destructive))
	proxy := registry.Func("tool.proxy", "Calls admin.wipe",
		func(ctx context.Context, _ struct{}) (json.RawMessage, error) {
			return r.Invoke(ctx, "admin.wipe", nil) // clamped to the caller's view
		}, registry.WithTags("agent"))
	if err := r.Add(wipe, proxy); err != nil {
		panic(err)
	}
	ctx := context.Background()
	agents := r.View(registry.Not(registry.Tagged("admin")))

	_, err := agents.Invoke(ctx, "admin.wipe", nil)
	fmt.Println("direct:", registry.CodeOf(err))
	_, err = agents.Invoke(ctx, "tool.proxy", nil)
	fmt.Println("nested via agent view:", registry.CodeOf(err))
	out, err := r.Invoke(ctx, "tool.proxy", nil)
	fmt.Println("nested via full view:", string(out), err)
	// Output:
	// direct: not_found
	// nested via agent view: not_found
	// nested via full view: "wiped" <nil>
}

// Every call is a job. A Long handler reports stage progress as it goes;
// replay keeps the latest update per stage and ends with the terminal event.
func ExampleView_Start() {
	r := registry.New()
	defer r.Close()
	build := registry.Func("demo.build", "Build something slowly",
		func(ctx context.Context, _ struct{}) (string, error) {
			for i := int64(1); i <= 3; i++ {
				registry.Progress(ctx, registry.Update{Stage: "compile", Done: i, Total: 3, Unit: "files"})
			}
			registry.Progress(ctx, registry.Update{Stage: "publish", Message: "writing output"})
			return "built", nil
		}, registry.AsLong(time.Minute))
	if err := r.Add(build); err != nil {
		panic(err)
	}
	job, err := r.Start(context.Background(), "demo.build", nil)
	if err != nil {
		panic(err)
	}
	out, err := job.Wait(context.Background())
	fmt.Println(string(out), err, job.Status().State)
	for ev := range job.Events(context.Background()) {
		if ev.Update != nil {
			fmt.Printf("%s %s %d/%d %s percent=%v\n", ev.State, ev.Update.Stage, ev.Update.Done, ev.Update.Total, ev.Update.Unit, ev.Update.Percent())
		} else {
			fmt.Println(ev.State)
		}
	}
	// Output:
	// "built" <nil> succeeded
	// queued
	// running
	// running compile 3/3 files percent=100
	// running publish 0/0  percent=-1
	// succeeded
}

// Close cancels every unfinished job and waits (up to JobDrainTimeout) for
// its runner to return, so terminal states are recorded before Close does.
func ExampleRegistry_Close() {
	r := registry.New(registry.JobDrainTimeout(5 * time.Second))
	started := make(chan struct{})
	wait := registry.Func("demo.wait", "Wait until cancelled",
		func(ctx context.Context, _ struct{}) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}, registry.AsLong(0))
	if err := r.Add(wait); err != nil {
		panic(err)
	}
	job, err := r.Start(context.Background(), "demo.wait", nil)
	if err != nil {
		panic(err)
	}
	<-started
	r.Close()
	st := job.Status()
	fmt.Println(st.State, st.Error.Code)
	// Output: cancelled cancelled
}

// Errors carry a closed Code and an optional Reason. Both survive %w
// wrapping and travel in the same envelope through every projection.
func ExampleReasonErrorf() {
	err := registry.ReasonErrorf(registry.Unavailable, registry.ReasonNoCapacity, "every emulator slot is in use")
	wrapped := fmt.Errorf("start emulator: %w", err)
	fmt.Println(registry.CodeOf(wrapped), registry.ReasonOf(wrapped), registry.CodeOf(wrapped).Retryable())
	env, _ := json.Marshal(registry.ErrorEnvelope{Error: registry.Info(wrapped)})
	fmt.Println(string(env))
	// Output:
	// unavailable no_capacity true
	// {"error":{"code":"unavailable","reason":"no_capacity","message":"every emulator slot is in use","retryable":true}}
}

var errMissing = errors.New("artifact missing")

// An adapter translates its own sentinel errors in one table.
func ExampleCodes() {
	codes := registry.Codes{errMissing: registry.NotFound}
	err := codes.Wrap(fmt.Errorf("artifact 7: %w", errMissing))
	fmt.Println(registry.CodeOf(err), errors.Is(err, errMissing))
	fmt.Println(registry.CodeOf(codes.Wrap(errors.New("disk on fire"))))
	// Output:
	// not_found true
	// internal
}

// Paginate implements the one pagination convention: follow Next until it
// is empty and every item is visited exactly once.
func ExamplePaginate() {
	items := []string{"a", "b", "c", "d", "e"}
	in := registry.PageIn{Limit: 2}
	for {
		page, err := registry.Paginate(items, in)
		if err != nil {
			panic(err)
		}
		fmt.Println(page.Items)
		if page.Next == "" {
			break
		}
		in.Cursor = page.Next
	}
	// Output:
	// [a b]
	// [c d]
	// [e]
}

// Group membership is by name: the longest registered group name that is
// the capability's name or a dotted prefix of it.
func ExampleRegistry_AddGroup() {
	r := registry.New()
	defer r.Close()
	noop := func(name string) registry.Capability {
		return registry.Func(name, name, func(ctx context.Context, _ struct{}) (string, error) { return "", nil })
	}
	if err := r.Add(noop("platform.overview.get"), noop("platform.scripts.run"), noop("misc.thing")); err != nil {
		panic(err)
	}
	if err := r.AddGroup(
		registry.Group{Name: "platform", Description: "Platform status and administration."},
		registry.Group{Name: "platform.scripts", Description: "Run and manage scripts."},
	); err != nil {
		panic(err)
	}
	for _, e := range r.List() {
		fmt.Printf("%s -> %q\n", e.Name, e.Group)
	}
	for _, g := range r.Groups() {
		fmt.Println(g.Name+":", g.Description)
	}
	// Output:
	// misc.thing -> ""
	// platform.overview.get -> "platform"
	// platform.scripts.run -> "platform.scripts"
	// platform: Platform status and administration.
	// platform.scripts: Run and manage scripts.
}
