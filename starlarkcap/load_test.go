package starlarkcap_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/script-services/modules"
	"github.com/craggy-io/script-services/script"
	"github.com/craggy-io/script-services/types"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

func TestLoadRejectsNamesOutsidePrefix(t *testing.T) {
	r := baseRegistry(t)
	before := len(r.All())
	src := `
capability(name = "apk.ok", run = lambda ctx, i: {})
capability(name = "apkx.sneaky", run = lambda ctx, i: {})
`
	_, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{})
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("err = %v, want invalid", err)
	}
	if want := "invalid: load failed:\n" + `apk.star:3:11: error: capability apkx.sneaky is outside the prefix "apk" (script.exec)`; err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if len(r.All()) != before {
		t.Fatal("rejected load mutated the registry")
	}
}

func TestLoadSupersessionPolicy(t *testing.T) {
	src := `
capability(name = "apk.get", supersedes = ["storage.artifact.get"], run = lambda ctx, i: {})
`
	t.Run("denied without policy", func(t *testing.T) {
		r := baseRegistry(t)
		_, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{})
		if registry.CodeOf(err) != registry.Denied {
			t.Fatalf("err = %v, want denied", err)
		}
		if !strings.Contains(err.Error(), "may not supersede storage.artifact.get") || !strings.Contains(err.Error(), "MaySupersede") {
			t.Fatalf("message = %v", err)
		}
		if _, ok := r.Get("apk.get"); ok {
			t.Fatal("denied capability installed")
		}
	})
	t.Run("allowed by policy", func(t *testing.T) {
		r := baseRegistry(t)
		l, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{MaySupersede: registry.Match("storage.*")})
		if err != nil {
			t.Fatal(err)
		}
		if len(l.Capabilities) != 1 || l.Capabilities[0] != "apk.get@1" {
			t.Fatalf("installed = %v", l.Capabilities)
		}
		e, _ := r.View(registry.All).Get("storage.artifact.get")
		if !e.Superseded() {
			t.Fatal("storage.artifact.get not superseded")
		}
	})
	t.Run("policy sees registered target", func(t *testing.T) {
		// The filter is tested against the registered capability, so tag
		// policies work: vault.secret.read is tagged admin.
		r := baseRegistry(t)
		admin := `capability(name = "apk.read", supersedes = ["vault.secret.read"], run = lambda ctx, i: {})`
		_, err := load(r, "apk", "apk.star", admin, starlarkcap.LoadOptions{MaySupersede: registry.Not(registry.Tagged("admin"))})
		if registry.CodeOf(err) != registry.Denied {
			t.Fatalf("err = %v, want denied", err)
		}
	})
	t.Run("inside prefix always allowed", func(t *testing.T) {
		r := baseRegistry(t)
		in := `
capability(name = "apk.old", run = lambda ctx, i: {})
capability(name = "apk.new", supersedes = ["apk.old"], run = lambda ctx, i: {})
`
		if _, err := load(r, "apk", "apk.star", in, starlarkcap.LoadOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLoadChecksBeforeInstall(t *testing.T) {
	r := baseRegistry(t)
	if _, err := load(r, "apk", "apk.star", `capability(name = "apk.keep", run = lambda ctx, i: {})`, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	before := len(r.All())
	var prints []string
	src := `
print("top level ran")
def _run(ctx, input):
    return helper(input)
capability(name = "apk.broken", run = _run)
capability(name = "apk.typo", run = _rnu)
`
	_, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{
		Print: func(pos script.Position, msg string) { prints = append(prints, msg) },
	})
	if registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("err = %v, want invalid", err)
	}
	for _, want := range []string{"load failed", "apk.star:4:12: error: undefined: helper (script.check)", "apk.star:6:37: error: undefined: _rnu (script.check)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%v", want, err)
		}
	}
	if len(prints) != 0 {
		t.Fatalf("file ran before passing its check: %q", prints)
	}
	if len(r.All()) != before {
		t.Fatal("rejected load mutated the registry")
	}
	if _, ok := r.Get("apk.keep"); !ok {
		t.Fatal("previous version lost")
	}
}

const sharedModuleV1 = `
def tag(x):
    return "v1:" + x
`

func TestLoadSharedModuleDigestAndOrigin(t *testing.T) {
	r := baseRegistry(t)
	files := map[string]string{"lib/common.star": sharedModuleV1}
	a := `
load("//lib/common.star", "tag")
capability(name = "a.tag", input = {"x": schema.string(required = True)}, run = lambda ctx, i: {"out": tag(i["x"])})
`
	b := `
load("lib/common.star", "tag")
capability(name = "b.tag", input = {"x": schema.string(required = True)}, run = lambda ctx, i: {"out": "b " + tag(i["x"])})
`
	loadBoth := func() (starlarkcap.Loaded, starlarkcap.Loaded) {
		t.Helper()
		fsys := fstest.MapFS{
			"bundle.yaml": {Data: manifest("test.caps",
				"{id: a, kind: capability.script@1, path: a.star}",
				"{id: b, kind: capability.script@1, path: b.star}")},
			"a.star": {Data: []byte(a)},
			"b.star": {Data: []byte(b)},
		}
		for p, text := range files {
			fsys[p] = &fstest.MapFile{Data: []byte(text)}
		}
		ls, err := starlarkcap.Load(context.Background(), r, []bundle.Source{bundle.Embedded("test", fsys)}, starlarkcap.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ls) != 2 || ls[0].Prefix != "a" || ls[1].Prefix != "b" {
			t.Fatalf("loaded = %+v", ls)
		}
		return ls[0], ls[1]
	}
	la, lb := loadBoth()
	if len(la.Modules) != 1 || la.Modules[0].Path != "lib/common.star" || len(lb.Modules) != 1 {
		t.Fatalf("modules a=%v b=%v", la.Modules, lb.Modules)
	}
	if la.Modules[0].Digest != lb.Modules[0].Digest {
		t.Fatal("one module, two digests")
	}
	if la.Digest != modules.Digest([]byte(a), []modules.Dep{{Spec: "//lib/common.star", Digest: la.Modules[0].Digest}}) || la.Digest == lb.Digest {
		t.Fatalf("digest a=%s b=%s", la.Digest, lb.Digest)
	}
	e, _ := r.Get("a.tag")
	if e.Origin != (registry.Origin{Kind: "script", Ref: "test.caps#a", Digest: la.Digest}) || la.Ref != "test.caps#a" {
		t.Fatalf("origin = %+v", e.Origin)
	}
	if d := e.Descriptor(); d.Origin == nil || d.Origin.Digest != la.Digest {
		t.Fatalf("descriptor origin = %+v", d.Origin)
	}
	out, err := r.Invoke(context.Background(), "b.tag", json.RawMessage(`{"x":"q"}`))
	if err != nil || string(out) != `{"out":"b v1:q"}` {
		t.Fatalf("b.tag = %s %v", out, err)
	}

	// Changing the module changes both files' digests on reload.
	files["lib/common.star"] = strings.Replace(sharedModuleV1, "v1:", "v2:", 1)
	la2, lb2 := loadBoth()
	if la2.Digest == la.Digest || lb2.Digest == lb.Digest || la2.Modules[0].Digest == la.Modules[0].Digest {
		t.Fatal("module change did not change digests")
	}
	out, err = r.Invoke(context.Background(), "a.tag", json.RawMessage(`{"x":"q"}`))
	if err != nil || string(out) != `{"out":"v2:q"}` {
		t.Fatalf("a.tag after reload = %s %v", out, err)
	}
	if e, _ := r.Get("a.tag"); e.Origin.Digest != la2.Digest {
		t.Fatalf("origin digest not updated: %+v", e.Origin)
	}
}

func TestLoadModuleFailureIsPositioned(t *testing.T) {
	r := baseRegistry(t)
	loadMain := func(main string) error {
		fsys := fstest.MapFS{
			"bundle.yaml":  {Data: manifest("test.caps", "{id: apk, kind: capability.script@1, path: apk.star}")},
			"apk.star":     {Data: []byte(main)},
			"lib/bad.star": {Data: []byte("def f():\n    return 1\nx = 1 // 0\n")},
		}
		_, err := starlarkcap.Load(context.Background(), r, []bundle.Source{bundle.Embedded("test", fsys)}, starlarkcap.LoadOptions{})
		return err
	}
	err := loadMain(`load("//lib/bad.star", "f")` + "\n")
	if registry.CodeOf(err) != registry.Invalid || !strings.Contains(err.Error(), "lib/bad.star:3") {
		t.Fatalf("err = %v", err)
	}
	var le *bundle.LoadError
	if !errors.As(err, &le) || le.Diagnostics[0].Path != "lib/bad.star" || le.Diagnostics[0].Line != 3 {
		t.Fatalf("diagnostics = %+v", le)
	}
	// A missing module is caught by the check.
	err = loadMain(`load("//lib/nope.star", "f")` + "\n")
	if registry.CodeOf(err) != registry.Invalid || !strings.Contains(err.Error(), "(script.check)") {
		t.Fatalf("err = %v", err)
	}
}

const declSrc = `package apk

// DecompileIn is what apk.decompile takes.
type DecompileIn struct {
	Artifact string ` + "`json:\"artifact\" jsonschema:\"artifact id\"`" + `
	Depth    *int   ` + "`json:\"depth,omitempty\"`" + `
}

// DecompileOut is what it returns.
type DecompileOut struct {
	Sources []string ` + "`json:\"sources\"`" + `
}
`

func TestLoadTypes(t *testing.T) {
	r := baseRegistry(t)
	pkg, err := types.Parse("apk.go", []byte(declSrc))
	if err != nil {
		t.Fatal(err)
	}
	src := `
capability(name = "apk.decompile", input = "DecompileIn", output = schema.type("DecompileOut"),
           run = lambda ctx, i: {"sources": [i["artifact"] + "/Main.java"]})
`
	if _, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{Types: pkg}); err != nil {
		t.Fatal(err)
	}
	e, _ := r.Get("apk.decompile")
	if len(e.Input.Required) != 1 || e.Input.Required[0] != "artifact" || e.Input.Properties["artifact"].Description != "artifact id" {
		t.Fatalf("input schema = %+v", e.Input)
	}
	if e.Output == nil || e.Output.Properties["sources"] == nil {
		t.Fatalf("output schema = %+v", e.Output)
	}
	out, err := r.Invoke(context.Background(), "apk.decompile", json.RawMessage(`{"artifact":"a.apk","depth":2}`))
	if err != nil || string(out) != `{"sources":["a.apk/Main.java"]}` {
		t.Fatalf("invoke = %s %v", out, err)
	}
	if _, err := r.Invoke(context.Background(), "apk.decompile", json.RawMessage(`{"depth":2}`)); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("missing artifact = %v", err)
	}
	// Without Types, a type name is not a schema.
	if _, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{}); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("load without Types = %v", err)
	}
}

func TestLoadPrintAndContextAttributes(t *testing.T) {
	r := baseRegistry(t)
	var prints []string
	src := `
print("loading")
def _who(ctx, input):
    print("running", ctx.job_id != "")
    return {"caller": ctx.caller["id"], "attrs": dir(ctx)}
capability(name = "apk.who", run = _who)
`
	if _, err := load(r, "apk", "apk.star", src, starlarkcap.LoadOptions{
		Print: func(pos script.Position, msg string) { prints = append(prints, msg+"@"+pos.File) },
	}); err != nil {
		t.Fatal(err)
	}
	ctx := registry.WithCaller(context.Background(), registry.Caller{ID: "user:me", Kind: "user"})
	out, err := r.Invoke(ctx, "apk.who", nil)
	if err != nil || string(out) != `{"attrs":["call","caller","job_id","progress","start"],"caller":"user:me"}` {
		t.Fatalf("who = %s %v", out, err)
	}
	if strings.Join(prints, "|") != "loading@apk.star|running True@apk.star" {
		t.Fatalf("prints = %q", prints)
	}
}

// Load refuses a prefix whose Replace would remove capabilities another
// file installed, instead of silently wiping them; the same contribution
// reloads freely. A prefix is one segment, so prefixes never nest.
func TestLoadRefusesPrefixOwnedByAnotherFile(t *testing.T) {
	r := baseRegistry(t)
	if _, err := loadFrom(r, "one", "a", "a.star", `capability(name = "a.b.y", run = lambda ctx, i: {})`, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := loadFrom(r, "two", "a", "a.star", `capability(name = "a.x", run = lambda ctx, i: {})`, starlarkcap.LoadOptions{})
	if registry.CodeOf(err) != registry.Conflict || !strings.Contains(err.Error(), "one#a") || !strings.Contains(err.Error(), "a.b.y@1") {
		t.Fatalf("err = %v, want a conflict naming one#a and a.b.y@1", err)
	}
	if _, ok := r.Get("a.b.y"); !ok {
		t.Fatal("one#a's capability was wiped")
	}
	if _, ok := r.Get("a.x"); ok {
		t.Fatal("refused load installed a.x")
	}
	// Reloading the owner, and replacing Go-registered names, still work.
	if _, err := loadFrom(r, "one", "a", "a.star", `capability(name = "a.b.y", version = 2, run = lambda ctx, i: {})`, starlarkcap.LoadOptions{}); err != nil {
		t.Fatalf("reload by the owner: %v", err)
	}
	if _, err := load(r, "storage", "storage.star", `capability(name = "storage.x", run = lambda ctx, i: {})`, starlarkcap.LoadOptions{}); err != nil {
		t.Fatalf("replacing Go-registered names: %v", err)
	}
	// After unloading the owner, the other bundle may load.
	if err := r.Replace("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFrom(r, "two", "a", "a.star", `capability(name = "a.x", run = lambda ctx, i: {})`, starlarkcap.LoadOptions{}); err != nil {
		t.Fatalf("load after unload: %v", err)
	}
}

// A dotted contribution id would nest one file's prefix inside another's,
// whose Replace would wipe it: it is rejected at compile time.
func TestLoadRejectsDottedPrefix(t *testing.T) {
	r := baseRegistry(t)
	_, err := load(r, "a.b", "a.b.star", `capability(name = "a.b.y", run = lambda ctx, i: {})`, starlarkcap.LoadOptions{})
	var le *bundle.LoadError
	if registry.CodeOf(err) != registry.Invalid || !errors.As(err, &le) || le.Diagnostics[0].Code != starlarkcap.CodePrefix ||
		!strings.Contains(err.Error(), "one file defines one top-level prefix") {
		t.Fatalf("err = %v, want invalid with %s", err, starlarkcap.CodePrefix)
	}
	if _, ok := r.Get("a.b.y"); ok {
		t.Fatal("a.b.y installed")
	}
}

// Load is all or nothing across files: when one is refused, the files
// installed before it are rolled back.
func TestLoadRollsBackEarlierFiles(t *testing.T) {
	r := baseRegistry(t)
	fsys := fstest.MapFS{
		"bundle.yaml": {Data: manifest("test.caps",
			"{id: a, kind: capability.script@1, path: a.star}",
			"{id: b, kind: capability.script@1, path: b.star}")},
		"a.star": {Data: []byte(`capability(name = "a.x", run = lambda ctx, i: {})`)},
		"b.star": {Data: []byte(`capability(name = "b.x", supersedes = ["vault.secret.read"], run = lambda ctx, i: {})`)},
	}
	_, err := starlarkcap.Load(context.Background(), r, []bundle.Source{bundle.Embedded("test", fsys)}, starlarkcap.LoadOptions{})
	if registry.CodeOf(err) != registry.Denied {
		t.Fatalf("err = %v, want denied", err)
	}
	if _, ok := r.Get("a.x"); ok {
		t.Fatal("a.x stayed installed after b was refused")
	}
}

// capability() is only valid while the file's top level runs: captured and
// called from a handler later, it fails instead of racing the loader.
func TestCapabilityAfterLoadFails(t *testing.T) {
	r := baseRegistry(t)
	src := `
define = capability
def _run(ctx, input):
    define(name = "a.late", run = _run)
    return {}
capability(name = "a.x", run = _run)
`
	if _, err := load(r, "a", "a.star", src, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.Invoke(context.Background(), "a.x", nil)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "capability() is only valid while a file loads") {
			t.Fatalf("late capability() = %v", err)
		}
	}
	if _, ok := r.Get("a.late"); ok {
		t.Fatal("a.late registered")
	}
}

// A bare supersedes ref covers every version of its target, so the policy
// must admit every registered version, not only the current one.
func TestLoadMaySupersedeChecksEveryVersion(t *testing.T) {
	r := baseRegistry(t)
	if err := r.Add(
		registry.Func("x.tool", "Tool v1", func(ctx context.Context, in struct{}) (struct{}, error) { return in, nil },
			registry.WithVersion(1), registry.WithTags("admin")),
		registry.Func("x.tool", "Tool v2", func(ctx context.Context, in struct{}) (struct{}, error) { return in, nil },
			registry.WithVersion(2)),
	); err != nil {
		t.Fatal(err)
	}
	policy := starlarkcap.LoadOptions{MaySupersede: registry.Not(registry.Tagged("admin"))}
	_, err := load(r, "apk", "apk.star", `capability(name = "apk.t", supersedes = ["x.tool"], run = lambda ctx, i: {})`, policy)
	if registry.CodeOf(err) != registry.Denied || !strings.Contains(err.Error(), "x.tool@1") {
		t.Fatalf("bare ref over an admin v1 = %v, want denied naming x.tool@1", err)
	}
	if e, _ := r.Get("x.tool@1"); e.Superseded() {
		t.Fatal("x.tool@1 superseded despite the policy")
	}
	if _, err := load(r, "apk", "apk.star", `capability(name = "apk.t", supersedes = ["x.tool@2"], run = lambda ctx, i: {})`, policy); err != nil {
		t.Fatalf("pinned ref to the admitted v2: %v", err)
	}
}

// LoadOptions.TypesDigest folds the types file into Loaded.Digest and
// Origin.Digest; without one the digest is the file's script digest.
func TestLoadDigestCoversTypes(t *testing.T) {
	r := baseRegistry(t)
	decl := func(field string) []byte {
		return []byte("package caps\n\ntype In struct {\n\t" + field + " string `json:\"" + strings.ToLower(field) + "\"`\n}\n")
	}
	src := `capability(name = "a.t", input = "In", run = lambda ctx, i: {})`
	digest := func(declSrc []byte, withDigest bool) string {
		t.Helper()
		pkg, err := types.Parse("types.go", declSrc)
		if err != nil {
			t.Fatal(err)
		}
		opts := starlarkcap.LoadOptions{Types: pkg}
		if withDigest {
			opts.TypesDigest = starlarkcap.TypesDigest(declSrc)
		}
		l, err := load(r, "a", "a.star", src, opts)
		if err != nil {
			t.Fatal(err)
		}
		if e := mustGet(t, r, "a.t"); e.Origin.Digest != l.Digest {
			t.Fatalf("origin digest %s != loaded digest %s", e.Origin.Digest, l.Digest)
		}
		return l.Digest
	}
	plain := digest(decl("X"), false)
	if plain != modules.Digest([]byte(src), nil) {
		t.Fatalf("without TypesDigest, digest = %s, want the script digest", plain)
	}
	x, y := digest(decl("X"), true), digest(decl("Y"), true)
	if x == plain || x == y || x != digest(decl("X"), true) {
		t.Fatalf("digests: plain %s, X %s, Y %s", plain, x, y)
	}
}
