package starlarkcap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/craggy-io/script-services/modules"
	"github.com/craggy-io/script-services/startest"
	"github.com/craggy-io/script-services/types"

	"github.com/craggy-io/capability-services/registry"
)

// WatchOptions configure Watch.
type WatchOptions struct {
	// Dir is a directory on disk holding the capability files and the
	// modules they load. Watch opens it with modules.DirRoot, an os.Root,
	// so no path, ".." or symbolic link reaches outside it, and closes it
	// when the watcher stops. Set exactly one of Dir and Root.
	Dir string
	// Root holds the capability files and modules as an fs.FS, such as an
	// embed.FS or an fstest.MapFS. For a directory on disk use Dir, or pass
	// an os.Root's FS (root, _ := os.OpenRoot(dir); root.FS()): os.DirFS
	// follows symbolic links out of the directory.
	Root fs.FS
	// Interval is how often the files are polled. Defaults to one second.
	Interval time.Duration
	// Load configures every load, including its supersession policy. When
	// Load.Types is nil and the root holds a types.go (TypesFile), Watch
	// parses it and loads every file with those types and their digest.
	Load LoadOptions
	// OnResult, when set, is called after every load, reload and unload,
	// and for every rejected file, from the watcher's goroutine (and from
	// Watch itself for the initial scan). A failed reload reports its error
	// while the previous version stays installed. A removed file reports a
	// Loaded with only Prefix set and the error from unloading it, if any.
	// OnResult may call Watcher.Close.
	OnResult func(prefix string, l Loaded, err error)
}

// TypesFile is the file in a watched root whose Go-syntax type declarations
// every capability file sees (see WatchOptions.Load).
const TypesFile = "types.go"

// Watcher hot-reloads a directory of capability files; see Watch.
type Watcher struct {
	r       *registry.Registry
	opts    WatchOptions
	root    fs.FS
	modules modules.Source
	cancel  context.CancelFunc
	done    chan struct{}
	ctx     context.Context
	loopID  atomic.Uint64 // goroutine id of the polling loop, for Close

	mu    sync.Mutex
	files map[string]*watched // by prefix
}

// watched is what the watcher knows about one capability file.
type watched struct {
	src       string            // hash of the file's text at the last attempt
	types     string            // hash of the types file at the last attempt ("" if none)
	deps      map[string]string // module path -> hash, from the last successful load
	installed bool              // a version of this file is in the registry
	failed    bool              // the last attempt failed
	rejected  bool              // the file's name is not a capability file's
	tree      string            // hash of every .star file at the last failed attempt
}

// Watch loads every capability file in the directory (opts.Dir or
// opts.Root) and keeps them current, polling every opts.Interval.
// Convention: each top-level "*.star" file other than "*_test.star"
// defines the capabilities under the prefix named by its stem ("apk.star"
// defines apk.*), and its load() statements resolve against the same root.
//
// One file defines one top-level prefix: a stem containing "." (such as
// "apk.extra.star") is rejected and reported through OnResult with
// registry.Invalid, because its prefix would nest inside another file's
// and that file's Replace would wipe it. Put apk.extra.* capabilities in
// apk.star. Load enforces the same ownership for files loaded directly.
//
// A file reloads when its text changes, when any module it loaded
// (transitively, per Loaded.Modules) changes, or when the types file
// changes. A failed reload keeps the previous version installed; a file
// that has never loaded retries whenever any .star file under the root
// changes. Removing a file unloads its prefix with r.Replace(prefix). Note
// that a file's stem claims the whole prefix: a file "jadx.star" replaces
// every jadx.* capability, Go-registered ones included.
//
// Types: unless opts.Load.Types is set, a TypesFile in the root is parsed
// (types.Parse) and passed to every load with its digest as
// LoadOptions.TypesDigest, so it counts in every Loaded.Digest; when it
// changes, appears or disappears every file reloads, and while it does not
// parse every (re)load fails with that error.
//
// The initial scan happens before Watch returns. Polling stops when ctx
// ends or Close is called; installed capabilities stay installed.
func Watch(ctx context.Context, r *registry.Registry, opts WatchOptions) (*Watcher, error) {
	var (
		root    fs.FS
		src     modules.Source
		closeFn func() error
	)
	switch {
	case opts.Dir != "" && opts.Root != nil:
		return nil, errors.New("starlarkcap.Watch: set one of WatchOptions.Dir and WatchOptions.Root, not both")
	case opts.Dir != "":
		rs, err := modules.DirRoot(opts.Dir)
		if err != nil {
			return nil, err
		}
		root, src, closeFn = rs.FS(), rs, rs.Close
	case opts.Root != nil:
		root, src = opts.Root, modules.Dir(opts.Root)
	default:
		return nil, errors.New("starlarkcap.Watch: WatchOptions.Dir or WatchOptions.Root is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := fs.ReadDir(root, "."); err != nil {
		if closeFn != nil {
			_ = closeFn()
		}
		return nil, err
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher{r: r, opts: opts, root: root, modules: src, cancel: cancel, done: make(chan struct{}), ctx: wctx, files: map[string]*watched{}}
	w.scan()
	go w.loop(closeFn)
	return w, nil
}

// Close stops the watcher and waits for an in-progress scan to finish. It
// may be called from OnResult: on the watcher's own goroutine it stops the
// watcher without waiting (the scan in progress ends after the callback
// returns, loading nothing more). Close is idempotent.
func (w *Watcher) Close() {
	w.cancel()
	if id := w.loopID.Load(); id != 0 && id == goroutineID() {
		return
	}
	<-w.done
}

// goroutineID returns the current goroutine's id, parsed from its stack
// header ("goroutine 42 [running]:"). Close uses it only to recognise the
// watcher's own goroutine.
func goroutineID() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		b = b[:i]
	}
	id, _ := strconv.ParseUint(string(b), 10, 64)
	return id
}

func (w *Watcher) loop(closeRoot func() error) {
	w.loopID.Store(goroutineID())
	defer close(w.done)
	if closeRoot != nil {
		defer closeRoot()
	}
	t := time.NewTicker(w.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
			w.scan()
		}
	}
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// snapshot reads every .star file under the root: path -> text.
func (w *Watcher) snapshot() map[string][]byte {
	out := map[string][]byte{}
	_ = fs.WalkDir(w.root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries count as absent
		}
		if d.IsDir() || !strings.HasSuffix(p, ".star") {
			return nil
		}
		if b, err := fs.ReadFile(w.root, p); err == nil {
			out[p] = b
		}
		return nil
	})
	return out
}

// isCapabilityFile reports whether p is a top-level capability file.
func isCapabilityFile(p string) bool {
	return !strings.Contains(p, "/") && strings.HasSuffix(p, ".star") && !startest.IsTestFile(p)
}

func (w *Watcher) scan() {
	w.mu.Lock()
	defer w.mu.Unlock()
	snap := w.snapshot()
	hashes := make(map[string]string, len(snap))
	paths := make([]string, 0, len(snap))
	for p, b := range snap {
		hashes[p] = hashOf(b)
		paths = append(paths, p)
	}
	sort.Strings(paths)
	treeH := sha256.New()
	for _, p := range paths {
		treeH.Write([]byte(p + "\x00" + hashes[p] + "\x00"))
	}
	tree := hex.EncodeToString(treeH.Sum(nil))
	opts, typesHash, typesErr := w.loadOptions()

	present := map[string]bool{}
	for _, p := range paths {
		if !isCapabilityFile(p) {
			continue
		}
		prefix := strings.TrimSuffix(p, ".star")
		present[prefix] = true
		st := w.files[prefix]
		if st != nil && !w.stale(st, hashes, tree, typesHash, p) {
			continue
		}
		if w.ctx.Err() != nil {
			return
		}
		if st == nil {
			st = &watched{}
			w.files[prefix] = st
		}
		st.src, st.types = hashes[p], typesHash
		if strings.Contains(prefix, ".") {
			st.rejected = true
			w.report(prefix, Loaded{}, registry.Errorf(registry.Invalid,
				"%s: a capability file's name may not contain \".\": one file defines one top-level prefix, so define %s.* capabilities in %s.star",
				p, prefix, prefix[:strings.IndexByte(prefix, '.')]))
			continue
		}
		var (
			l   Loaded
			err = typesErr
		)
		if err == nil {
			l, err = Load(w.ctx, w.r, prefix, Source{Filename: p, Src: snap[p], Modules: w.modules}, opts)
		}
		if err != nil {
			st.failed, st.tree = true, tree
		} else {
			st.failed, st.tree, st.installed = false, "", true
			st.deps = map[string]string{}
			for _, m := range l.Modules {
				st.deps[m.Path] = hashes[m.Path] // "" if it vanished meanwhile: reloads next scan
			}
		}
		w.report(prefix, l, err)
	}
	for prefix, st := range w.files {
		if present[prefix] {
			continue
		}
		delete(w.files, prefix)
		if st.rejected {
			continue
		}
		var err error
		if st.installed {
			err = w.r.Replace(prefix)
		}
		w.report(prefix, Loaded{Prefix: prefix}, err)
	}
}

// loadOptions returns the options for this scan's loads: WatchOptions.Load,
// plus the root's TypesFile unless the host set Types itself. It also
// returns the types file's hash ("" when absent or unused) and its parse
// error, which fails every load until the file is fixed.
func (w *Watcher) loadOptions() (LoadOptions, string, error) {
	opts := w.opts.Load
	if opts.Types != nil {
		return opts, "", nil
	}
	src, err := fs.ReadFile(w.root, TypesFile)
	if err != nil {
		return opts, "", nil // absent or unreadable: no types
	}
	pkg, err := types.Parse(TypesFile, src)
	if err != nil {
		return opts, hashOf(src), registry.WrapCode(registry.Invalid, err)
	}
	opts.Types, opts.TypesDigest = pkg, TypesDigest(src)
	return opts, hashOf(src), nil
}

// stale reports whether the file at p needs (re)loading.
func (w *Watcher) stale(st *watched, hashes map[string]string, tree, typesHash, p string) bool {
	if st.src != hashes[p] {
		return true
	}
	if st.rejected {
		return false
	}
	if st.types != typesHash {
		return true
	}
	if st.failed {
		return st.tree != tree
	}
	for dep, h := range st.deps {
		if hashes[dep] != h {
			return true
		}
	}
	return false
}

func (w *Watcher) report(prefix string, l Loaded, err error) {
	if w.opts.OnResult != nil {
		w.opts.OnResult(prefix, l, err)
	}
}
