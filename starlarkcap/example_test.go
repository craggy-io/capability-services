package starlarkcap_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"
	"time"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/script-services/types"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

type artifactIn struct {
	ID string `json:"id"`
}

// exampleRegistry has a storage capability and a legacy decompiler that a
// script will supersede.
func exampleRegistry() *registry.Registry {
	r := registry.New()
	get := registry.Func("storage.artifact.get", "Get an artifact",
		func(ctx context.Context, in artifactIn) (map[string]string, error) {
			return map[string]string{"id": in.ID, "name": in.ID + ".apk"}, nil
		}, registry.WithEffects(registry.ReadOnly), registry.WithTags("agent"))
	jadx := registry.Func("jadx.decompile", "Decompile (legacy)",
		func(ctx context.Context, in artifactIn) (string, error) { return "jadx", nil },
		registry.WithTags("agent"))
	fail := registry.Func("storage.artifact.lock", "Lock an artifact",
		func(ctx context.Context, in artifactIn) (string, error) {
			return "", registry.Errorf(registry.Conflict, "artifact %s is already locked", in.ID)
		})
	if err := r.Add(get, jadx, fail); err != nil {
		panic(err)
	}
	return r
}

const apkStar = `
load("//lib/names.star", "sources_for")

def _decompile(ctx, input):
    ctx.progress(stage = "decompile", done = 1, total = 1, unit = "files")
    art = ctx.call("storage.artifact.get", id = input["artifact"])
    return {"sources": sources_for(art["name"])}

capability(
    name = "apk.decompile",
    description = "Decompile an APK",
    supersedes = ["jadx.decompile"],
    input = {"artifact": schema.string(required = True)},
    tags = ["agent"],
    run = _decompile,
)
`

// apkBundle is a bundle holding apk.star, declared as the capability file
// for the prefix "apk", and a module it loads.
var apkBundle = fstest.MapFS{
	"bundle.yaml": {Data: []byte(`apiVersion: craggy.io/bundle/v1
metadata: {id: example.apk, version: 1.0.0}
contributions:
  - {id: apk, kind: capability.script@1, path: apk.star}
`)},
	"apk.star":       {Data: []byte(apkStar)},
	"lib/names.star": {Data: []byte("def sources_for(name):\n    return [name + '/Main.java']\n")},
}

// Load loads bundles, checks and runs each capability file they declare,
// and installs what it defines under its prefix, the contribution id.
// Superseding a name outside the prefix needs the host's MaySupersede
// policy; installed capabilities record their Origin.
func ExampleLoad() {
	r := exampleRegistry()
	defer r.Close()
	ctx := context.Background()
	sources := []bundle.Source{bundle.Embedded("apk", apkBundle)} // bundle.Dir(dir) for a folder

	_, err := starlarkcap.Load(ctx, r, sources, starlarkcap.LoadOptions{})
	fmt.Println(registry.CodeOf(err))

	loaded, err := starlarkcap.Load(ctx, r, sources, starlarkcap.LoadOptions{MaySupersede: registry.Match("jadx.*")})
	if err != nil {
		panic(err)
	}
	l := loaded[0]
	fmt.Println(l.Prefix, l.Ref, l.Capabilities, l.Modules[0].Path, strings.HasPrefix(l.Digest, "sha256:"))
	e, _ := r.Get("apk.decompile")
	fmt.Println(e.Origin.Kind, e.Origin.Ref, e.Origin.Digest == l.Digest)
	old, _ := r.Get("jadx.decompile")
	fmt.Println("jadx superseded by", old.SupersededBy)

	out, err := r.Invoke(ctx, "apk.decompile", json.RawMessage(`{"artifact":"art-1"}`))
	fmt.Println(string(out), err)

	if err := r.Replace("apk"); err != nil { // unload
		panic(err)
	}
	old, _ = r.Get("jadx.decompile")
	fmt.Println("after unload:", old.Superseded())
	// Output:
	// denied
	// apk example.apk#apk [apk.decompile@1] lib/names.star true
	// script example.apk#apk true
	// jadx superseded by [apk.decompile]
	// {"sources":["art-1.apk/Main.java"]} <nil>
	// after unload: false
}

// oneFile is a bundle "example.caps" holding src as the capability file
// for the prefix "apk".
func oneFile(src string) []bundle.Source {
	return []bundle.Source{bundle.Embedded("caps", fstest.MapFS{
		"bundle.yaml": {Data: []byte(`apiVersion: craggy.io/bundle/v1
metadata: {id: example.caps, version: 1.0.0}
contributions:
  - {id: apk, kind: capability.script@1, path: apk.star}
`)},
		"apk.star": {Data: []byte(src)},
	})}
}

// The file is checked before anything runs: a typo rejects the load with
// positioned diagnostics, and the registry keeps what it had.
func ExampleLoad_checkFirst() {
	r := exampleRegistry()
	defer r.Close()
	src := `
print("never printed")
capability(name = "apk.info", run = _infoo)
`
	_, err := starlarkcap.Load(context.Background(), r, oneFile(src), starlarkcap.LoadOptions{})
	fmt.Println(err)
	var le *bundle.LoadError
	fmt.Println(errors.As(err, &le), le.Diagnostics[0].Line)
	// Output:
	// invalid: bundle: load failed with 1 problem(s):
	//   example.caps:apk.star:3:37: error: undefined: _infoo (script.check)
	// true 3
}

const declarations = `package apk

// DecompileIn is what apk.decompile takes.
type DecompileIn struct {
	Artifact string ` + "`json:\"artifact\" jsonschema:\"artifact id\"`" + `
}
`

// With LoadOptions.Types, a capability can name a Go-syntax type
// declaration as its input or output schema.
func ExampleLoad_types() {
	r := exampleRegistry()
	defer r.Close()
	pkg, err := types.Parse("apk.go", []byte(declarations))
	if err != nil {
		panic(err)
	}
	src := `capability(name = "apk.name", input = "DecompileIn", run = lambda ctx, i: {"artifact": i["artifact"]})`
	if _, err := starlarkcap.Load(context.Background(), r, oneFile(src), starlarkcap.LoadOptions{Types: pkg}); err != nil {
		panic(err)
	}
	e, _ := r.Get("apk.name")
	fmt.Println(e.Input.Required, e.Input.Properties["artifact"].Description)
	_, err = r.Invoke(context.Background(), "apk.name", json.RawMessage(`{}`))
	fmt.Println(registry.CodeOf(err))
	// Output:
	// [artifact] artifact id
	// invalid
}

// A failure inside a script-defined capability surfaces as internal: the
// script, not its caller, is at fault. The nested capability's code stays
// visible in the message.
func ExampleLoad_errors() {
	r := exampleRegistry()
	defer r.Close()
	src := `capability(name = "apk.lock", input = {"id": schema.string(required = True)},
           run = lambda ctx, i: ctx.call("storage.artifact.lock", id = i["id"]))`
	if _, err := starlarkcap.Load(context.Background(), r, oneFile(src), starlarkcap.LoadOptions{}); err != nil {
		panic(err)
	}
	_, err := r.Invoke(context.Background(), "apk.lock", json.RawMessage(`{"id":"art-1"}`))
	fmt.Println(registry.CodeOf(err))
	fmt.Println(err)
	// Output:
	// internal
	// internal: apk.lock: apk.star:2:41: conflict: artifact art-1 is already locked
}

// Exec runs a script against the view in its context: the module body,
// then run(input). Hooks see print() and every capability call with its
// call site; an unhandled capability failure keeps that capability's code.
func ExampleExec() {
	r := exampleRegistry()
	defer r.Close()
	ctx := registry.WithView(context.Background(), r.View(registry.Tagged("agent")))
	src := `
art = storage.artifact.get(id = input["id"])
print("got", art["name"])

def run(input):
    return {"name": art["name"]}
`
	hooks := starlarkcap.Hooks{
		Print: func(pos starlarkcap.Position, msg string) { fmt.Printf("%s:%d: %s\n", pos.File, pos.Line, msg) },
		Call: func(ev starlarkcap.CallEvent) {
			if ev.Done {
				fmt.Printf("call %s at line %d done\n", ev.Ref, ev.Pos.Line)
			}
		},
	}
	lim := starlarkcap.Limits{MaxSteps: 100_000, Timeout: 10 * time.Second}
	res, err := starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "nb.star", Source: []byte(src), Input: map[string]any{"id": "art-1"}}, lim, hooks)
	fmt.Println(string(res.Value), err)

	// storage.artifact.lock is not in the agent view: not_found, positioned.
	_, err = starlarkcap.Exec(ctx, starlarkcap.Program{Filename: "bad.star", Source: []byte(`call("storage.artifact.lock", id = "x")`)}, lim, starlarkcap.Hooks{})
	var se *starlarkcap.Error
	if errors.As(err, &se) {
		fmt.Println(se.Code, se.Pos.File, se.Pos.Line, registry.CodeOf(err))
		fmt.Println(se.Message)
		fmt.Println(err) // the code once, then the position
	}
	// Output:
	// call storage.artifact.get@1 at line 2 done
	// nb.star:3: got art-1.apk
	// {"name":"art-1.apk"} <nil>
	// not_found bad.star 1 not_found
	// not_found: capability "storage.artifact.lock" not found
	// not_found: bad.star:1:5: capability "storage.artifact.lock" not found
}

// Check validates a script against a view without running anything, with
// nearest-name suggestions.
func ExampleCheck() {
	r := exampleRegistry()
	defer r.Close()
	src := `
art = storage.artifact.gte(id = "art-1")
other = storage.artifact.get(idd = "art-1")
`
	for _, d := range starlarkcap.Check(r.View(registry.All), "nb.star", []byte(src)) {
		fmt.Printf("%d:%d %s: %s\n", d.Pos.Line, d.Pos.Col, d.Severity, d.Message)
	}
	// Output:
	// 2:7 error: unknown capability storage.artifact.gte (did you mean storage.artifact.get?)
	// 3:9 error: storage.artifact.get has no input "idd" (did you mean id?)
	// 3:9 error: storage.artifact.get requires input "id"
}

// Watch loads bundles into a bundle.Live, installs every capability file
// they declare, and reinstalls on change. Test files declared beside them
// are left for Test. The initial install finishes before Watch returns.
func ExampleWatch() {
	r := exampleRegistry()
	defer r.Close()
	dir, err := os.MkdirTemp("", "caps")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			panic(err)
		}
	}
	write("bundle.yaml", `apiVersion: craggy.io/bundle/v1
metadata: {id: example.hello, version: 1.0.0}
contributions:
  - {id: hello, kind: capability.script@1, path: hello.star}
  - {id: hello-test, kind: craggy.script-test@1, path: hello_test.star}
`)
	write("hello.star", `capability(name = "hello.world", run = lambda ctx, i: {"hi": True})`)
	write("hello_test.star", "def test_hi():\n    assert.true(hello.world()[\"hi\"])\n") // not installed

	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Sources:  []bundle.Source{bundle.Dir(dir)}, // read through an os.Root; symbolic links are refused
		Interval: time.Hour,                        // this example only shows the initial install
		OnResult: func(prefix string, l starlarkcap.Loaded, err error) {
			fmt.Println(prefix, l.Capabilities, err)
		},
	})
	if err != nil {
		panic(err)
	}
	defer w.Close()
	e, _ := r.Get("hello.world")
	fmt.Println(e.Origin.Ref)
	// Output:
	// hello [hello.world@1] <nil>
	// example.hello#hello
}
