package starlarkcap_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/craggy-io/foundation-kit/bundle"

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
	mu     sync.Mutex
	all    []result
	errors []error
}

func (rc *recorder) onError(err error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.errors = append(rc.errors, err)
}

func (rc *recorder) errorCount() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.errors)
}

func (rc *recorder) lastError() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.errors) == 0 {
		return nil
	}
	return rc.errors[len(rc.errors)-1]
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

// writeManifest writes dir's bundle.yaml: bundle id, then contributions.
func writeManifest(t *testing.T, dir, id string, contributions ...string) {
	t.Helper()
	writeFile(t, dir, "bundle.yaml", string(manifest(id, contributions...)))
}

const (
	contribA     = "{id: a, kind: capability.script@1, path: a.star}"
	contribATest = "{id: a-test, kind: craggy.script-test@1, path: a_test.star}"
)

// watchDir watches the bundle in dir every 10ms, recording results and
// errors.
func watchDir(t *testing.T, r *registry.Registry, dir string, rec *recorder) *starlarkcap.Watcher {
	t.Helper()
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Sources: []bundle.Source{bundle.Dir(dir)}, Interval: 10 * time.Millisecond, OnResult: rec.on, OnError: rec.onError,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

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
	writeManifest(t, dir, "test.watch", contribA, contribATest)
	writeFile(t, dir, "lib/common.star", sharedModuleV1)
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "", 1))
	writeFile(t, dir, "a_test.star", "def test_x():\n    pass\n")
	var rec recorder
	w := watchDir(t, r, dir, &rec)

	// Load: the initial install is done when Watch returns.
	if got := invokeTag(r); got != "v1:q" {
		t.Fatalf("initial = %q", got)
	}
	first := rec.snapshot()
	if len(first) != 1 || first[0].prefix != "a" || first[0].err != nil || len(first[0].l.Modules) != 1 {
		t.Fatalf("initial results = %+v", first)
	}
	e, _ := r.Get("a.tag")
	if e.Origin.Kind != "script" || e.Origin.Ref != "test.watch#a" || e.Origin.Digest != first[0].l.Digest {
		t.Fatalf("origin = %+v", e.Origin)
	}

	// Edit: the new version replaces the old.
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "/edited", 1))
	waitFor(t, "edit to load", func() bool { return invokeTag(r) == "v1:q/edited" })

	// Broken edit: the reload fails whole and is reported; the previous
	// version stays installed.
	writeFile(t, dir, "a.star", "capability(name = \"a.tag\", run = \n")
	waitFor(t, "broken edit to be reported", func() bool { return rec.errorCount() > 0 })
	if err := rec.lastError(); registry.CodeOf(err) != registry.Invalid || !strings.Contains(err.Error(), "a.star:") {
		t.Fatalf("reload error = %v, want invalid and positioned", err)
	}
	if got := invokeTag(r); got != "v1:q/edited" {
		t.Fatalf("after broken edit = %q, want the previous version", got)
	}

	// Fix it, then change only the module: the file reloads.
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "/fixed", 1))
	waitFor(t, "fix to load", func() bool { return invokeTag(r) == "v1:q/fixed" })
	writeFile(t, dir, "lib/common.star", strings.Replace(sharedModuleV1, "v1:", "v2:", 1))
	waitFor(t, "module change to reload", func() bool { return invokeTag(r) == "v2:q/fixed" })

	// Undeclaring the file unloads the prefix.
	writeManifest(t, dir, "test.watch", contribATest)
	if err := os.Remove(filepath.Join(dir, "a.star")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "removal to unload", func() bool { _, ok := r.Get("a.tag"); return !ok })
	waitFor(t, "removal to be reported", func() bool {
		return rec.any(func(x result) bool { return x.prefix == "a" && x.err == nil && len(x.l.Capabilities) == 0 })
	})
	if rec.any(func(x result) bool { return x.prefix != "a" }) {
		t.Fatalf("a test contribution was installed: %+v", rec.snapshot())
	}

	// Close stops reloading.
	w.Close()
	n := len(rec.snapshot())
	writeFile(t, dir, "b.star", `capability(name = "b.x", run = lambda ctx, i: {})`)
	writeManifest(t, dir, "test.watch", "{id: b, kind: capability.script@1, path: b.star}")
	time.Sleep(100 * time.Millisecond)
	if _, ok := r.Get("b.x"); ok || len(rec.snapshot()) != n {
		t.Fatal("watcher kept loading after Close")
	}
}

// The first load must succeed; later, a reload that fails keeps everything
// and the watcher recovers once the problem is fixed.
func TestWatchFirstLoadAndRecovery(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeManifest(t, dir, "test.watch", contribA)
	writeFile(t, dir, "a.star", strings.Replace(watchedFile, "%s", "", 1))
	_, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Sources: []bundle.Source{bundle.Dir(dir)}})
	var le *bundle.LoadError
	if registry.CodeOf(err) != registry.Invalid || !errors.As(err, &le) {
		t.Fatalf("Watch with a missing module = %v, want invalid with a *bundle.LoadError", err)
	}
	if _, ok := r.Get("a.tag"); ok {
		t.Fatal("a failed first load installed a.tag")
	}

	writeFile(t, dir, "lib/common.star", sharedModuleV1)
	var rec recorder
	watchDir(t, r, dir, &rec)
	if got := invokeTag(r); got != "v1:q" {
		t.Fatalf("initial = %q", got)
	}
	if err := os.Remove(filepath.Join(dir, "lib", "common.star")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "missing module to be reported", func() bool { return rec.errorCount() > 0 })
	writeFile(t, dir, "lib/common.star", strings.Replace(sharedModuleV1, "v1:", "v2:", 1))
	waitFor(t, "recovery once the module is back", func() bool { return invokeTag(r) == "v2:q" })
}

func TestWatchAppliesPolicyAndStopsWithContext(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeManifest(t, dir, "test.watch", "{id: apk, kind: capability.script@1, path: apk.star}")
	writeFile(t, dir, "apk.star", `capability(name = "apk.get", supersedes = ["storage.artifact.get"], run = lambda ctx, i: {})`)
	ctx, cancel := context.WithCancel(context.Background())
	var rec recorder
	w, err := starlarkcap.Watch(ctx, r, starlarkcap.WatchOptions{Sources: []bundle.Source{bundle.Dir(dir)}, Interval: 10 * time.Millisecond, OnResult: rec.on})
	if err != nil {
		t.Fatal(err)
	}
	if res := rec.snapshot(); len(res) != 1 || registry.CodeOf(res[0].err) != registry.Denied {
		t.Fatalf("results = %+v, want one denied install", res)
	}
	// A refused install is retried with the next catalog.
	writeFile(t, dir, "apk.star", `capability(name = "apk.get", run = lambda ctx, i: {})`)
	waitFor(t, "the fixed file to install", func() bool { _, ok := r.Get("apk.get"); return ok })
	cancel()
	w.Close() // returns once the cancelled watcher has stopped

	if _, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{}); err == nil {
		t.Fatal("Watch without sources succeeded")
	}
}

// One file defines one prefix: a dotted contribution id would nest inside
// another file's prefix, so it fails the load.
func TestWatchRejectsDottedPrefix(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeManifest(t, dir, "test.watch", "{id: a.b, kind: capability.script@1, path: a.b.star}")
	writeFile(t, dir, "a.b.star", `capability(name = "a.b.y", run = lambda ctx, i: {})`)
	_, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Sources: []bundle.Source{bundle.Dir(dir)}})
	if registry.CodeOf(err) != registry.Invalid || !strings.Contains(err.Error(), string(starlarkcap.CodePrefix)) {
		t.Fatalf("err = %v, want invalid with %s", err, starlarkcap.CodePrefix)
	}
}

// Close may be called from OnResult, on the watcher's own goroutine: it
// stops the watcher without waiting for itself.
func TestWatcherCloseFromOnResult(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeManifest(t, dir, "test.watch")
	var (
		mu      sync.Mutex
		w       *starlarkcap.Watcher
		results int
	)
	returned := make(chan struct{})
	w0, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Sources: []bundle.Source{bundle.Dir(dir)}, Interval: 10 * time.Millisecond,
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
	writeManifest(t, dir, "test.watch", contribA)
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
	writeManifest(t, dir, "test.watch", contribA, "{id: b, kind: capability.script@1, path: b.star}")
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if results != n {
		t.Fatal("watcher kept loading after closing itself")
	}
}

// A types asset types the file that declares it, counts in its digest, and
// reinstalls it when it changes; a file that does not declare it is left
// alone.
func TestWatchReloadsOnTypesChange(t *testing.T) {
	r := baseRegistry(t)
	dir := t.TempDir()
	writeManifest(t, dir, "test.watch",
		"{id: a, kind: capability.script@1, path: a.star, assets: [assets/types.go]}",
		"{id: b, kind: capability.script@1, path: b.star}")
	writeFile(t, dir, "assets/types.go", "package caps\n\n// In is the input.\ntype In struct {\n\tX string `json:\"x\"`\n}\n")
	writeFile(t, dir, "a.star", `capability(name = "a.t", input = "In", run = lambda ctx, i: {})`)
	writeFile(t, dir, "b.star", `capability(name = "b.plain", run = lambda ctx, i: {})`)
	var rec recorder
	watchDir(t, r, dir, &rec)
	e, ok := r.Get("a.t")
	if !ok {
		t.Fatalf("a.star did not load: %+v", rec.snapshot())
	}
	if _, has := e.Capability.Input.Properties["y"]; has {
		t.Fatal("y before the types change")
	}
	digestA, digestB := e.Origin.Digest, mustGet(t, r, "b.plain").Origin.Digest

	writeFile(t, dir, "assets/types.go", "package caps\n\n// In is the input.\ntype In struct {\n\tX string `json:\"x\"`\n\tY int    `json:\"y\"`\n}\n")
	waitFor(t, "types change to reload a.star", func() bool {
		e, ok := r.Get("a.t")
		_, has := e.Capability.Input.Properties["y"]
		return ok && has
	})
	if mustGet(t, r, "a.t").Origin.Digest == digestA {
		t.Fatal("a.star's digest ignores its types asset")
	}
	if mustGet(t, r, "b.plain").Origin.Digest != digestB {
		t.Fatal("b.star's digest changed with a types asset it does not declare")
	}
	var bInstalls int
	for _, x := range rec.snapshot() {
		if x.prefix == "b" {
			bInstalls++
		}
	}
	if bInstalls != 1 {
		t.Fatalf("b.star installed %d times, want once: unchanged files are not reinstalled", bInstalls)
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

// bundle.Dir reads through an os.Root and refuses symbolic links: neither a
// capability file nor a module reached by a link out of the directory
// loads.
func TestWatchDirRefusesSymlinkEscape(t *testing.T) {
	r := baseRegistry(t)
	base := t.TempDir()
	dir := filepath.Join(base, "caps")
	writeFile(t, base, "outside.star", `capability(name = "evil.x", run = lambda ctx, i: {})`)
	writeFile(t, base, "secret.star", "SECRET = 'leaked'\n")
	writeManifest(t, dir, "test.watch", contribA, "{id: evil, kind: capability.script@1, path: evil.star}")
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
	_, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Sources: []bundle.Source{bundle.Dir(dir)}})
	if registry.CodeOf(err) != registry.Invalid || !strings.Contains(err.Error(), string(bundle.CodeSymlinkForbidden)) {
		t.Fatalf("err = %v, want invalid with %s", err, bundle.CodeSymlinkForbidden)
	}
	if _, ok := r.Get("evil.x"); ok {
		t.Fatal("a capability file outside the directory was loaded through a symlink")
	}
	if _, ok := r.Get("a.x"); ok {
		t.Fatal("a module outside the directory was loaded through a symlink")
	}
}

// bundle.Embedded takes any fs.FS holding a bundle.yaml, such as an
// embed.FS or a MapFS.
func TestWatchEmbedded(t *testing.T) {
	r := baseRegistry(t)
	fsys := fstest.MapFS{
		"bundle.yaml":     {Data: manifest("test.watch", contribA)},
		"lib/common.star": {Data: []byte(sharedModuleV1)},
		"a.star":          {Data: []byte(strings.Replace(watchedFile, "%s", "", 1))},
	}
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Sources: []bundle.Source{bundle.Embedded("caps", fsys)}, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := invokeTag(r); got != "v1:q" {
		t.Fatalf("a.tag = %q", got)
	}
}

// A prefix may move from one watched bundle to another: the watcher's own
// earlier install is not a conflict.
func TestWatchPrefixMovesBetweenBundles(t *testing.T) {
	r := baseRegistry(t)
	one, two := t.TempDir(), t.TempDir()
	writeManifest(t, one, "test.one", contribA)
	writeFile(t, one, "a.star", `capability(name = "a.x", run = lambda ctx, i: {"from": "one"})`)
	writeManifest(t, two, "test.two")
	writeFile(t, two, "a.star", `capability(name = "a.x", run = lambda ctx, i: {"from": "two"})`)
	var rec recorder
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Sources: []bundle.Source{bundle.Dir(one), bundle.Dir(two)}, Interval: 10 * time.Millisecond, OnResult: rec.on, OnError: rec.onError,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if e := mustGet(t, r, "a.x"); e.Origin.Ref != "test.one#a" {
		t.Fatalf("origin = %+v", e.Origin)
	}
	writeManifest(t, one, "test.one")
	writeManifest(t, two, "test.two", contribA)
	waitFor(t, "a to move to test.two", func() bool {
		e, ok := r.Get("a.x")
		return ok && e.Origin.Ref == "test.two#a"
	})
	if rec.any(func(x result) bool { return registry.CodeOf(x.err) == registry.Conflict }) {
		t.Fatalf("moving a prefix was a conflict: %+v", rec.snapshot())
	}
}

// Bundles added to and removed from a root install and unload their files
// without restarting the watcher; a missing root is refused.
func TestWatchRoots(t *testing.T) {
	r := baseRegistry(t)
	if _, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{Roots: []string{filepath.Join(t.TempDir(), "missing")}}); err == nil {
		t.Fatal("missing root accepted")
	}
	root := t.TempDir()
	add := func(name string) {
		t.Helper()
		dir := filepath.Join(root, name)
		writeManifest(t, dir, "test."+name, "{id: "+name+", kind: capability.script@1, path: "+name+".star}")
		writeFile(t, dir, name+".star", `capability(name = "`+name+`.x", run = lambda ctx, i: {"from": "`+name+`"})`)
	}
	add("one")
	writeFile(t, filepath.Join(root, "not-a-bundle"), "readme.txt", "no bundle.yaml: skipped")
	var rec recorder
	w, err := starlarkcap.Watch(context.Background(), r, starlarkcap.WatchOptions{
		Roots: []string{root}, Interval: 10 * time.Millisecond, OnResult: rec.on, OnError: rec.onError,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	if _, ok := r.Get("one.x"); !ok {
		t.Fatal("one.x not installed at Watch")
	}
	add("two")
	waitFor(t, "added bundle to install", func() bool { _, ok := r.Get("two.x"); return ok })
	// On Windows a reload in progress holds the folder open: retry.
	waitFor(t, "bundle folder to be removed", func() bool { return os.RemoveAll(filepath.Join(root, "one")) == nil })
	waitFor(t, "removed bundle to unload", func() bool { _, ok := r.Get("one.x"); return !ok })
	if _, ok := r.Get("two.x"); !ok {
		t.Fatal("two.x unloaded with one")
	}
	if n := rec.errorCount(); n != 0 {
		t.Fatalf("%d reload errors: %v", n, rec.lastError())
	}
}
