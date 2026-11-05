package starlarkcap

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/capability-services/registry"
)

// WatchOptions configure Watch.
type WatchOptions struct {
	// Sources are the bundles to watch, each with its bundle.yaml: usually
	// bundle.Dir(dir), which reads through an os.Root and rejects
	// symbolic links, or bundle.Embedded for an fs.FS. Bundles that only
	// serve modules to the others (declared dependencies) go here too.
	// Required.
	Sources []bundle.Source
	// Interval is how often the bundles are reloaded (bundle.Live.Run).
	// Defaults to one second.
	Interval time.Duration
	// Load configures compiling and installing, including the supersession
	// policy.
	Load LoadOptions
	// OnResult, when set, is called after every install, reinstall and
	// unload of a capability file, and for every refused install, from the
	// watcher's goroutine (and from Watch itself for the initial catalog).
	// A refused reinstall reports its error while the previous version
	// stays installed. A file that left the catalog reports a Loaded with
	// only Prefix set and the error from unloading it, if any. OnResult
	// may call Watcher.Close.
	OnResult func(prefix string, l Loaded, err error)
	// OnError, when set, receives every reload that failed to load (an
	// invalid bundle.yaml, a file that fails its check or top level): a
	// registry.Invalid error listing the diagnostics, wrapping the
	// *bundle.LoadError. The previous catalog stays current, so every
	// installed file stays as it was. It is called from the watcher's
	// goroutine and may call Watcher.Close.
	OnError func(err error)
}

// Watcher hot-reloads bundles of capability files; see Watch.
type Watcher struct {
	r      *registry.Registry
	opts   WatchOptions
	live   *bundle.Live
	cancel context.CancelFunc
	done   chan struct{}
	ctx    context.Context
	inCall atomic.Bool // a callback is running

	mu    sync.Mutex
	files map[string]*watched // by prefix
}

// watched is what the watcher knows about one prefix.
type watched struct {
	ref    string // Origin.Ref of the last attempt
	digest string // digest of the last attempt
	ok     bool   // the last attempt installed
	owner  string // Origin.Ref of what is installed, "" for nothing
}

// Watch loads opts.Sources into a bundle.Live, compiled as Load compiles,
// installs every capability file in the catalog, and keeps them current:
// the Live reloads every opts.Interval, and each new catalog is applied to
// the registry.
//
// Applying a catalog installs each file whose digest or contribution
// changed (Load's rules: supersession policy, one file per prefix),
// and unloads, with r.Replace(prefix), each prefix whose file left the
// catalog. A file whose install was refused is retried with every new
// catalog. A file's prefix claims the whole prefix: a file "jadx"
// replaces every jadx.* capability, Go-registered ones included. Moving a
// prefix from one watched bundle to another is a reinstall.
//
// Reloads are whole-catalog (bundle.Live): when any bundle fails to load,
// for example one file has a syntax error, nothing changes, every file
// stays as it was, and OnError reports why. A bundle.yaml is required in
// every source, and a file is watched only when the manifest declares it;
// declare a contribution to add a file and remove it to unload one.
//
// The first load must succeed: Watch returns its error (registry.Invalid
// with the diagnostics) and starts nothing. The initial install happens
// before Watch returns. Reloading stops when ctx ends or Close is called;
// installed capabilities stay installed.
func Watch(ctx context.Context, r *registry.Registry, opts WatchOptions) (*Watcher, error) {
	if len(opts.Sources) == 0 {
		return nil, errors.New("starlarkcap.Watch: WatchOptions.Sources is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	o := bundle.Options{Sources: slices.Clone(opts.Sources), Compilers: compilers(opts.Load)}
	live, err := bundle.NewLive(ctx, func(context.Context) (bundle.Options, error) { return o, nil })
	if err != nil {
		return nil, loadError(err)
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &Watcher{r: r, opts: opts, live: live, cancel: cancel, done: make(chan struct{}), ctx: wctx, files: map[string]*watched{}}
	w.apply(live.Catalog())
	unsubscribe := live.Subscribe(func(_, cat *bundle.Catalog) { w.apply(cat) })
	go w.loop(unsubscribe)
	return w, nil
}

// Close stops the watcher and waits for a reload in progress to finish.
// Called while an OnResult or OnError callback runs (from the callback
// itself, typically), it stops the watcher without waiting: the catalog
// being applied installs and unloads nothing more after the callback
// returns. Close is idempotent.
func (w *Watcher) Close() {
	w.cancel()
	if !w.inCall.Load() {
		<-w.done
	}
}

func (w *Watcher) loop(unsubscribe func()) {
	defer close(w.done)
	defer unsubscribe()
	_ = w.live.Run(w.ctx, w.opts.Interval, func(err error) {
		if w.opts.OnError != nil {
			w.callback(func() { w.opts.OnError(loadError(err)) })
		}
	})
}

// apply brings the registry in line with cat.
func (w *Watcher) apply(cat *bundle.Catalog) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defs := bundle.All(cat, Script)
	sort.Slice(defs, func(i, j int) bool { return defs[i].Value.Prefix() < defs[j].Value.Prefix() })
	present := map[string]bool{}
	for _, def := range defs {
		f := def.Value
		prefix, ref := f.Prefix(), f.info.Ref
		present[prefix] = true
		st := w.files[prefix]
		if st != nil && st.ok && st.ref == ref && st.digest == f.info.Digest {
			continue
		}
		if w.ctx.Err() != nil {
			return
		}
		if st == nil {
			st = &watched{}
			w.files[prefix] = st
		}
		l, err := install(w.r, f, w.opts.Load.MaySupersede, st.owner)
		st.ref, st.digest, st.ok = ref, f.info.Digest, err == nil
		if err == nil {
			st.owner = ref
		}
		w.report(prefix, l, err)
	}
	gone := make([]string, 0)
	for prefix := range w.files {
		if !present[prefix] {
			gone = append(gone, prefix)
		}
	}
	sort.Strings(gone)
	for _, prefix := range gone {
		if w.ctx.Err() != nil {
			return // Close from an unload's OnResult stops the rest too
		}
		st := w.files[prefix]
		delete(w.files, prefix)
		var err error
		if st.owner != "" {
			err = w.r.Replace(prefix)
		}
		w.report(prefix, Loaded{Prefix: prefix}, err)
	}
}

func (w *Watcher) report(prefix string, l Loaded, err error) {
	if w.opts.OnResult != nil {
		w.callback(func() { w.opts.OnResult(prefix, l, err) })
	}
}

func (w *Watcher) callback(fn func()) {
	w.inCall.Store(true)
	defer w.inCall.Store(false)
	fn()
}
