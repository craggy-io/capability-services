package starlarkcap_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

// writeFile replaces dir/name atomically, so the watcher never reads a
// half-written file.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type result struct {
	prefix string
	l      starlarkcap.Loaded
	err    error
}

type recorder struct {
	mu  sync.Mutex
	all []result
}

func (rc *recorder) on(prefix string, l starlarkcap.Loaded, err error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.all = append(rc.all, result{prefix, l, err})
}

func (rc *recorder) snapshot() []result {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]result(nil), rc.all...)
}

func (rc *recorder) any(f func(result) bool) bool {
	for _, r := range rc.snapshot() {
		if f(r) {
			return true
		}
	}
	return false
}

const watchedFile = `
load("//lib/common.star", "tag")
capability(name = "a.tag", input = {"x": schema.string(required = True)},
           run = lambda ctx, i: {"out": tag(i["x"]) + "%s"})
`

func invokeTag(r *registry.Registry) string {
	out, err := r.Invoke(context.Background(), "a.tag", json.RawMessage(`{"x":"q"}`))
	if err != nil {
		return "error: " + err.Error()
	}
	var v struct{ Out string }
	_ = json.Unmarshal(out, &v)
	return v.Out
}

func TestWatchLifecycle(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "lib/common.star", sharedModuleV1)
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "", 1))
	writeFile(t, dir, "a_test.star", "def test_x():\n    pass\n")
	var rec recorder
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Dir: dir, Interval: 10 * time.Millisecond, OnResult: rec.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Load: the initial scan is done when Watch returns.
	if got := invokeTag(r); got != "v1:q" {
		t.Fatalf("initial = %q", got)
	}
	first := rec.snapshot()
	if len(first) != 1 || first[0].prefix != "a" || first[0].err != nil || len(first[0].l.Modules) != 1 {
		t.Fatalf("initial results = %+v", first)
	}
	e, _ := r.Get("a.tag")
	if e.Origin.Kind != "script" || e.Origin.Ref != "a.star" || e.Origin.Digest != first[0].l.Digest {
		t.Fatalf("origin = %+v", e.Origin)
	}

	// Edit: the new version replaces the old.
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "/edited", 1))
	waitFor(t, "edit to load", func() bool { return invokeTag(r) == "v1:q/edited" })

	// Broken edit: reported, previous version stays installed.
	writeFile(t, dir, "a.star", "capability(name = \"a.tag\", run = \n")
	waitFor(t, "broken edit to be reported", func() bool {
		return rec.any(func(x result) bool { return x.prefix == "a" && x.err != nil })
	})
	if got := invokeTag(r); got != "v1:q/edited" {
		t.Fatalf("after broken edit = %q, want the previous version", got)
	}

	// Fix it, then change only the module: the file reloads.
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "/fixed", 1))
	waitFor(t, "fix to load", func() bool { return invokeTag(r) == "v1:q/fixed" })
	writeFile(t, dir, "lib/common.star", strings.Replace(sharedModuleV1, "v1:", "v2:", 1))
	waitFor(t, "module change to reload", func() bool { return invokeTag(r) == "v2:q/fixed" })

	// Removal unloads the prefix.
	if err := os.Remove(filepath.Join(dir, "a.star")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "removal to unload", func() bool { _, ok := r.Get("a.tag"); return !ok })
	waitFor(t, "removal to be reported", func() bool {
		return rec.any(func(x result) bool { return x.prefix == "a" && x.err == nil && len(x.l.Capabilities) == 0 })
	})
	if rec.any(func(x result) bool { return x.prefix == "a_test" }) {
		t.Fatal("a _test.star file was loaded as capabilities")
	}

	// Close stops polling.
	w.Close()
	n := len(rec.snapshot())
	writeFile(t, dir, "b.star", `capability(name = "b.x", run = lambda ctx, i: {})`)
	time.Sleep(100 * time.Millisecond)
	if _, ok := r.Get("b.x"); ok || len(rec.snapshot()) != n {
		t.Fatal("watcher kept loading after Close")
	}
}

func TestWatchRetriesFailedFileWhenAModuleAppears(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "", 1))
	var rec recorder
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Dir: dir, Interval: 10 * time.Millisecond, OnResult: rec.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if res := rec.snapshot(); len(res) != 1 || res[0].err == nil {
		t.Fatalf("missing module should fail the first load: %+v", res)
	}
	writeFile(t, dir, "lib/common.star", sharedModuleV1)
	waitFor(t, "retry after the module appears", func() bool { return invokeTag(r) == "v1:q" })
}

func TestWatchAppliesPolicyAndStopsWithContext(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "apk.star", `capability(name = "apk.get", supersedes = ["storage.artifact.get"], run = lambda ctx, i: {})`)
	ctx, cancel := context.WithCancel(context.Background())
	var rec recorder
	w, err := starlarkcap.Watch(ctx, r, starlarkcap.WatchOptions{Dir: dir, Interval: 10 * time.Millisecond, OnResult: rec.on})
	if err != nil {
		t.Fatal(err)
	}
	if res := rec.snapshot(); len(res) != 1 || registry.CodeOf(res[0].err) != registry.Denied {
		t.Fatalf("results = %+v, want one denied load", res)
	}
	cancel()
	w.Close() // returns once the cancelled watcher has stopped

	if _, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{}); err == nil {
		t.Fatal("Watch without a root succeeded")
	}
}

// One file defines one prefix: a.b.star beside a.star would have its a.b.*
// capabilities wiped by a.star's Replace("a") and never come back, so Watch
// rejects a stem containing "." and reports why.
func TestWatchRejectsNestedPrefixFiles(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "a.star", `capability(name = "a.x", run = lambda ctx, i: {"from": "a.star"})`)
	writeFile(t, dir, "a.b.star", `capability(name = "a.b.y", run = lambda ctx, i: {"from": "a.b.star"})`)
	var rec recorder
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Dir: dir, Interval: 10 * time.Millisecond, OnResult: rec.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	time.Sleep(50 * time.Millisecond) // a few more scans
	if _, ok := r.Get("a.x"); !ok {
		t.Fatal("a.star was not loaded")
	}
	var nested []result
	for _, x := range rec.snapshot() {
		if x.prefix == "a.b" {
			nested = append(nested, x)
		}
	}
	if len(nested) != 1 || registry.CodeOf(nested[0].err) != registry.Invalid ||
		!strings.Contains(nested[0].err.Error(), "one file defines one top-level prefix") {
		t.Fatalf("a.b.star results = %+v, want one invalid report about prefixes", nested)
	}
	if _, ok := r.Get("a.b.y"); ok {
		t.Fatal("a.b.star was loaded")
	}
}

// Close may be called from OnResult, on the watcher's own goroutine: it
// stops the watcher without waiting for itself.
func TestWatcherCloseFromOnResult(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	var (
		mu      sync.Mutex
		w       *starlarkcap.Watcher
		results int
	)
	returned := make(chan struct{})
	w0, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Dir: dir, Interval: 10 * time.Millisecond,
		OnResult: func(prefix string, l starlarkcap.Loaded, err error) {
			mu.Lock()
			self := w
			results++
			mu.Unlock()
			if self != nil {
				self.Close()
				close(returned)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	w = w0
	mu.Unlock()
	writeFile(t, dir, "a.star", `capability(name = "a.x", run = lambda ctx, i: {})`)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close called from OnResult did not return")
	}
	stopped := make(chan struct{})
	go func() { w0.Close(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a later Close did not return")
	}
	mu.Lock()
	n := results
	mu.Unlock()
	writeFile(t, dir, "b.star", `capability(name = "b.x", run = lambda ctx, i: {})`)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if results != n {
		t.Fatal("watcher kept loading after closing itself")
	}
}

// The conventional types.go in the root types every file, counts in each
// file's digest, and reloads every file when it changes.
func TestWatchReloadsOnTypesChange(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "types.go", "package caps\n\n// In is the input.\ntype In struct {\n\tX string `json:\"x\"`\n}\n")
	writeFile(t, dir, "a.star", `capability(name = "a.t", input = "In", run = lambda ctx, i: {})`)
	writeFile(t, dir, "b.star", `capability(name = "b.plain", run = lambda ctx, i: {})`)
	var rec recorder
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Dir: dir, Interval: 10 * time.Millisecond, OnResult: rec.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	e, ok := r.Get("a.t")
	if !ok {
		t.Fatalf("a.star did not load: %+v", rec.snapshot())
	}
	if _, has := e.Capability.Input.Properties["y"]; has {
		t.Fatal("y before the types change")
	}
	digestA, digestB := e.Origin.Digest, mustGet(t, r, "b.plain").Origin.Digest

	writeFile(t, dir, "types.go", "package caps\n\n// In is the input.\ntype In struct {\n\tX string `json:\"x\"`\n\tY int    `json:\"y\"`\n}\n")
	waitFor(t, "types change to reload a.star", func() bool {
		e, ok := r.Get("a.t")
		_, has := e.Capability.Input.Properties["y"]
		return ok && has
	})
	waitFor(t, "types change to reload b.star", func() bool { return mustGet(t, r, "b.plain").Origin.Digest != digestB })
	if mustGet(t, r, "a.t").Origin.Digest == digestA {
		t.Fatal("a.star's digest ignores the types file")
	}
}

func mustGet(t *testing.T, r *registry.Registry, ref string) registry.Entry {
	t.Helper()
	e, ok := r.Get(ref)
	if !ok {
		t.Fatalf("%s not registered", ref)
	}
	return e
}

// WatchOptions.Dir reads through an os.Root: neither a capability file nor
// a module reached by a symbolic link out of the directory loads.
func TestWatchDirRefusesSymlinkEscape(t *testing.T) {
	r := baseRegistry(t)
	base := t.TempDir()
	dir := filepath.Join(base, "caps")
	writeFile(t, base, "outside.star", `capability(name = "evil.x", run = lambda ctx, i: {})`)
	writeFile(t, base, "secret.star", "SECRET = 'leaked'\n")
	writeFile(t, dir, "a.star", "load(\"//lib/secret.star\", \"SECRET\")\ncapability(name = \"a.x\", run = lambda ctx, i: {\"s\": SECRET})\n")
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		filepath.Join(dir, "evil.star"):          filepath.Join("..", "outside.star"),
		filepath.Join(dir, "lib", "secret.star"): filepath.Join("..", "..", "secret.star"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	var rec recorder
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Dir: dir, Interval: 10 * time.Millisecond, OnResult: rec.on})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, ok := r.Get("evil.x"); ok {
		t.Fatal("a capability file outside the directory was loaded through a symlink")
	}
	if _, ok := r.Get("a.x"); ok {
		t.Fatal("a module outside the directory was loaded through a symlink")
	}
	if !rec.any(func(x result) bool { return x.prefix == "a" && x.err != nil }) {
		t.Fatalf("a.star's failure was not reported: %+v", rec.snapshot())
	}
	if _, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Dir: dir, Root: os.DirFS(dir)}); err == nil {
		t.Fatal("Watch accepted both Dir and Root")
	}
}

// WatchOptions.Root takes any fs.FS, such as an embed.FS or a MapFS.
func TestWatchRootFS(t *testing.T) {
	r := baseRegistry(t)
	fsys := fstest.MapFS{
		"lib/common.star": {Data: []byte(sharedModuleV1)},
		"a.star":          {Data: []byte(strings.Replace(watchedFile, "%s", "", 1))},
	}
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Root: fsys, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := invokeTag(r); got != "v1:q" {
		t.Fatalf("a.tag = %q", got)
	}
}
