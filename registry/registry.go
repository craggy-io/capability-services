package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

type resolvedCap struct {
	capab  Capability
	inRes  *jsonschema.Resolved
	outRes *jsonschema.Resolved
	seq    uint64 // unique per registration: tells a replacement from the original
}

var registrations atomic.Uint64

// snapshot is immutable after publication. A call already running keeps the
// capability it started with.
type snapshot struct {
	caps    map[capKey]resolvedCap
	byName  map[string][]int // name -> versions ascending
	current map[string]int   // name -> highest version
	supBy   map[capKey][]string
	groups  map[string]Group // carried across every capability change
}

func emptySnapshot() *snapshot {
	return &snapshot{caps: map[capKey]resolvedCap{}, byName: map[string][]int{}, current: map[string]int{}, supBy: map[capKey][]string{}, groups: map[string]Group{}}
}

// Registry holds an atomic pointer to an immutable snapshot, plus the job
// table, readiness state, watch broadcast and per-tag limit semaphores.
type Registry struct {
	cfg config

	snap atomic.Pointer[snapshot]
	// allView is View(All); Registry delegates view methods to it.
	allView *View

	writeMu sync.Mutex // serializes Add/Replace/Remove

	readyMu sync.RWMutex
	ready   map[capKey]readyState

	changeMu sync.Mutex
	changed  chan struct{} // closed & replaced on every change

	jobsMu         sync.RWMutex
	jobs           map[string]*Job
	closed         bool // set by Close under jobsMu; Start then refuses
	jobJournalMu   sync.Mutex
	jobJournalPath string
	jobJournalErr  error
	sharedErr      error                    // under jobJournalMu
	sharedPut      map[string]sharedPutMark // under jobJournalMu: last shared write per running job

	sems map[string]chan struct{} // tag -> semaphore (built in New)

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	loopGID  atomic.Int64 // goroutine id of loop(); Close from it must not wg.Wait itself
	// stopCtx is cancelled by Close; readiness checks derive from it so a
	// Close issued from inside a Check unblocks the loop waiting on that
	// same check instead of deadlocking against wg.Wait.
	stopCtx    context.Context
	stopCancel context.CancelFunc

	runners runnerSet // job runner goroutines still executing
}

// runnerSet counts running job runners so Close can wait for them. Start
// adds a runner under jobsMu together with inserting its job, so Close's
// snapshot, cancel and wait cover every runner. Unlike a sync.WaitGroup the
// wait can be abandoned after a timeout without leaking a goroutine.
type runnerSet struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n drops to 0; nil while n == 0
}

func (s *runnerSet) add() {
	s.mu.Lock()
	if s.n == 0 {
		s.idle = make(chan struct{})
	}
	s.n++
	s.mu.Unlock()
}

func (s *runnerSet) done() {
	s.mu.Lock()
	s.n--
	if s.n == 0 {
		close(s.idle)
		s.idle = nil
	}
	s.mu.Unlock()
}

// wait blocks until no runner is executing or timeout elapses, and reports
// whether all runners finished. A timeout <= 0 does not wait.
func (s *runnerSet) wait(timeout time.Duration) bool {
	s.mu.Lock()
	idle := s.idle
	s.mu.Unlock()
	if idle == nil {
		return true
	}
	if timeout <= 0 {
		return false
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-t.C:
		return false
	}
}

type readyState struct {
	ready bool
	err   string
}

// New builds an empty registry.
func New(opts ...Option) *Registry {
	cfg := defaultConfig()
	for _, o := range opts {
		if o.regFn != nil {
			o.regFn(&cfg)
		}
	}
	// A nil Middleware is a host bug that would otherwise panic every
	// Start; drop it here rather than at call time.
	mw := cfg.middleware[:0]
	for _, m := range cfg.middleware {
		if m != nil {
			mw = append(mw, m)
		}
	}
	cfg.middleware = mw
	r := &Registry{
		cfg: cfg, ready: map[capKey]readyState{},
		changed: make(chan struct{}), jobs: map[string]*Job{}, sharedPut: map[string]sharedPutMark{},
		sems: map[string]chan struct{}{}, stopCh: make(chan struct{}),
	}
	r.stopCtx, r.stopCancel = context.WithCancel(context.Background())
	for tag, n := range cfg.limits {
		r.sems[tag] = make(chan struct{}, n)
	}
	r.snap.Store(emptySnapshot())
	r.allView = &View{reg: r, filter: All}
	// The loop also purges finished jobs, so it runs when either periodic
	// checks or job expiry is enabled; an invoke-only host with
	// CheckEvery(0) must still honour KeepJobs.
	if cfg.checkEvery > 0 || cfg.keepJobs > 0 {
		r.wg.Add(1)
		go r.loop()
	}
	return r
}

// Close stops the background checker, cancels all non-terminal jobs and
// waits for their runners to return, so every job's terminal state (and
// journal record) is written before Close returns. From the moment Close
// starts, Start and Invoke fail with unavailable (reason ReasonClosed), so
// a call racing Close either fails or creates a job Close cancels and waits
// for. The wait is bounded by JobDrainTimeout (default 10s): a handler that
// ignores cancellation for longer is abandoned, its job may still finish
// (and journal) later, and Close returns anyway. It is idempotent; later
// calls wait again, under the same bound. The registry should not be used
// after Close; listings still work, but no call starts.
func (r *Registry) Close() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
		r.stopCancel()
		r.jobsMu.Lock()
		r.closed = true
		jobs := make([]*Job, 0, len(r.jobs))
		for _, j := range r.jobs {
			jobs = append(jobs, j)
		}
		r.jobsMu.Unlock()
		for _, j := range jobs {
			if !j.Status().State.Terminal() {
				j.Cancel()
			}
		}
	})
	// A Check running on the loop goroutine may itself call Close (the
	// watcher-style self-close); waiting for the loop there would wait
	// for ourselves forever.
	if goid() != r.loopGID.Load() {
		r.wg.Wait()
	}
	r.runners.wait(r.cfg.drainTimeout)
}

// goid returns the current goroutine's id, parsed from its stack header.
// It exists only so Close can tell "called from the loop goroutine" apart
// from any other caller; 0 never matches a real goroutine.
func goid() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := bytes.Fields(buf[:n]) // "goroutine <id> [...]"
	if len(fields) < 2 {
		return 0
	}
	id, err := strconv.ParseInt(string(fields[1]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func (r *Registry) loop() {
	defer r.wg.Done()
	r.loopGID.Store(goid())
	var checkC, purgeC <-chan time.Time
	if r.cfg.checkEvery > 0 {
		t := time.NewTicker(r.cfg.checkEvery)
		defer t.Stop()
		checkC = t.C
	}
	if r.cfg.keepJobs > 0 {
		every := r.cfg.keepJobs / 10
		if every < 10*time.Millisecond {
			every = 10 * time.Millisecond
		}
		if every > 30*time.Second {
			every = 30 * time.Second
		}
		t := time.NewTicker(every)
		defer t.Stop()
		purgeC = t.C
	}
	for {
		select {
		case <-r.stopCh:
			return
		case <-checkC:
			r.RefreshChecks(context.Background())
			r.purgeJobs()
		case <-purgeC:
			r.purgeJobs()
		}
	}
}

func (r *Registry) notify() {
	r.changeMu.Lock()
	old := r.changed
	r.changed = make(chan struct{})
	r.changeMu.Unlock()
	close(old)
}

func (r *Registry) changedChan() <-chan struct{} {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	return r.changed
}

// View returns a live, filtered view. A nil filter means All.
func (r *Registry) View(f Filter) *View {
	if f == nil {
		f = All
	}
	return &View{reg: r, filter: f}
}

// normalize validates and canonicalizes a capability for registration:
// version 0 becomes 1, slices are copied, schemas are resolved (a bad
// schema fails here, at Add, with Invalid).
func normalize(c Capability) (capKey, resolvedCap, error) {
	if err := validateName(c.Name); err != nil {
		return capKey{}, resolvedCap{}, err
	}
	if c.Version < 0 {
		return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: negative version %d", c.Name, c.Version)
	}
	if c.Handler == nil {
		return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: nil handler", c.Name)
	}
	for _, ref := range c.Supersedes {
		if _, _, _, ok := ParseRef(ref); !ok {
			return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: invalid supersedes ref %q", c.Name, ref)
		}
	}
	for _, ref := range c.Requires {
		if _, _, _, ok := ParseRef(ref); !ok {
			return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: invalid requires ref %q", c.Name, ref)
		}
	}
	for _, dep := range c.Dependencies {
		if strings.TrimSpace(dep.Kind) == "" || strings.TrimSpace(dep.Name) == "" {
			return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: dependencies need kind and name", c.Name)
		}
	}
	accepts, err := normalizeSubjects(c.Name, "accepts", c.Accepts)
	if err != nil {
		return capKey{}, resolvedCap{}, err
	}
	emits, err := normalizeSubjects(c.Name, "emits", c.Emits)
	if err != nil {
		return capKey{}, resolvedCap{}, err
	}
	c.Version = normVersion(c.Version)
	// Duplicated tags are meaningless and, for a limited tag, harmful:
	// runJob acquires the tag's semaphore once per occurrence.
	c.Tags = dedupeStrings(c.Tags)
	c.Supersedes = dedupeStrings(c.Supersedes)
	c.Requires = dedupeStrings(c.Requires)
	c.Dependencies = append([]Dependency(nil), c.Dependencies...)
	c.Accepts = accepts
	c.Emits = emits
	// The snapshot is immutable, so it must own its schemas: a caller
	// mutating the schema it registered must not rewrite validation.
	c.Input = cloneSchema(c.Input)
	c.Output = cloneSchema(c.Output)
	inRes, err := resolveSchema(c.Input)
	if err != nil {
		return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: input schema: %v", c.Name, err)
	}
	outRes, err := resolveSchema(c.Output)
	if err != nil {
		return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: output schema: %v", c.Name, err)
	}
	if len(c.Examples) > 0 && c.Input == nil {
		return capKey{}, resolvedCap{}, Errorf(Invalid, "%s: examples need an input schema", c.Name)
	}
	if c.Examples, err = normalizeExamples(c.Name, c.Examples, inRes); err != nil {
		return capKey{}, resolvedCap{}, err
	}
	return capKey{c.Name, c.Version}, resolvedCap{c, inRes, outRes, registrations.Add(1)}, nil
}

// buildSnapshot derives versions and supersession from what's registered —
// nothing about them is stored separately.
func buildSnapshot(caps map[capKey]resolvedCap) (*snapshot, error) {
	s := emptySnapshot()
	s.caps = caps
	for k := range caps {
		s.byName[k.name] = append(s.byName[k.name], k.version)
	}
	for name, vs := range s.byName {
		sort.Ints(vs)
		s.current[name] = vs[len(vs)-1]
	}
	// Supersession graph: edge from superseder to each registered target.
	graph := map[capKey][]capKey{}
	for k, rc := range caps {
		for _, ref := range rc.capab.Supersedes {
			name, ver, pinned, _ := ParseRef(ref)
			var targets []capKey
			if pinned {
				if _, ok := caps[capKey{name, ver}]; ok {
					targets = append(targets, capKey{name, ver})
				}
			} else {
				for _, v := range s.byName[name] {
					targets = append(targets, capKey{name, v})
				}
			}
			for _, t := range targets {
				graph[k] = append(graph[k], t)
				label := k.name
				if s.current[k.name] != k.version {
					label = refOf(k.name, k.version)
				}
				s.supBy[t] = appendUnique(s.supBy[t], label)
			}
		}
	}
	for _, v := range s.supBy {
		sort.Strings(v)
	}
	if err := detectCycle(graph); err != nil {
		return nil, err
	}
	return s, nil
}

// dedupeStrings copies in without duplicates, preserving order. Nil
// stays nil.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// cloneSchema deep-copies a schema: every pointer, slice and map it reaches
// is copied, so the clone shares nothing mutable with s. Pointers already
// copied are reused, so shared sub-schemas stay shared and a hand-built
// cyclic schema is copied as the same cycle.
func cloneSchema(s *jsonschema.Schema) *jsonschema.Schema {
	if s == nil {
		return nil
	}
	return deepCopy(reflect.ValueOf(s), map[uintptr]reflect.Value{}).Interface().(*jsonschema.Schema)
}

// deepCopy copies v recursively; seen maps each copied pointer to its copy.
func deepCopy(v reflect.Value, seen map[uintptr]reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		if c, ok := seen[v.Pointer()]; ok {
			return c
		}
		c := reflect.New(v.Elem().Type())
		seen[v.Pointer()] = c
		c.Elem().Set(deepCopy(v.Elem(), seen))
		return c
	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		c.Set(v)
		for i := range v.NumField() {
			if c.Field(i).CanSet() {
				c.Field(i).Set(deepCopy(v.Field(i), seen))
			}
		}
		return c
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			c.Index(i).Set(deepCopy(v.Index(i), seen))
		}
		return c
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			c.SetMapIndex(it.Key(), deepCopy(it.Value(), seen))
		}
		return c
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		c := reflect.New(v.Type()).Elem()
		c.Set(deepCopy(v.Elem(), seen))
		return c
	}
	return v
}

// cloneCapability returns a copy of c sharing nothing mutable with it:
// slices, example payloads and schemas are all copied. Entry/Descriptor
// consumers and view filters receive clones so handling a listing can
// never rewrite the immutable snapshot — or race its readers.
func cloneCapability(c Capability) Capability {
	c.Tags = append([]string(nil), c.Tags...)
	c.Supersedes = append([]string(nil), c.Supersedes...)
	c.Requires = append([]string(nil), c.Requires...)
	c.Dependencies = append([]Dependency(nil), c.Dependencies...)
	c.Accepts = append([]Subject(nil), c.Accepts...)
	c.Emits = append([]Subject(nil), c.Emits...)
	if c.Examples != nil {
		ex := make([]Example, len(c.Examples))
		for i, e := range c.Examples {
			e.Input = append(json.RawMessage(nil), e.Input...)
			ex[i] = e
		}
		c.Examples = ex
	}
	c.Input = cloneSchema(c.Input)
	c.Output = cloneSchema(c.Output)
	return c
}

func appendUnique(s []string, v string) []string {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}

func detectCycle(graph map[capKey][]capKey) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[capKey]int{}
	var visit func(capKey) error
	visit = func(n capKey) error {
		switch color[n] {
		case grey:
			return Errorf(Invalid, "supersession cycle involving %s", refOf(n.name, n.version))
		case black:
			return nil
		}
		color[n] = grey
		for _, m := range graph[n] {
			if err := visit(m); err != nil {
				return err
			}
		}
		color[n] = black
		return nil
	}
	for n := range graph {
		if err := visit(n); err != nil {
			return err
		}
	}
	return nil
}

// Add registers capabilities. It returns an error (and registers nothing)
// on a duplicate name@version (Conflict), a bad name/schema/ref (Invalid),
// or a supersession cycle (Invalid). A supersession rule may name a
// capability that isn't registered yet; it takes effect when it appears.
func (r *Registry) Add(caps ...Capability) error {
	if len(caps) == 0 {
		return nil
	}
	norm := make(map[capKey]resolvedCap, len(caps))
	for _, c := range caps {
		k, rc, err := normalize(c)
		if err != nil {
			return err
		}
		if _, dup := norm[k]; dup {
			return Errorf(Conflict, "duplicate capability %s in Add", refOf(k.name, k.version))
		}
		norm[k] = rc
	}
	// Readiness checks run host code: never under writeMu, where a Check
	// that itself registers capabilities would self-deadlock and a slow
	// Check would block every other registration.
	results := r.runChecksFor(norm)
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.snap.Load()
	for k := range norm {
		if _, exists := cur.caps[k]; exists {
			return Errorf(Conflict, "capability %s already registered", refOf(k.name, k.version))
		}
	}
	merged := make(map[capKey]resolvedCap, len(cur.caps)+len(norm))
	for k, v := range cur.caps {
		merged[k] = v
	}
	for k, v := range norm {
		merged[k] = v
	}
	return r.publishLocked(merged, results)
}

// Replace atomically swaps everything under prefix: all capabilities whose
// name is prefix or under prefix+"." are removed and caps installed. Every
// new capability must itself live under prefix. Replace(prefix) with no
// capabilities unloads the group. This one primitive replaces any plugin
// lifecycle; a failed validation leaves the registry unchanged.
func (r *Registry) Replace(prefix string, caps ...Capability) error {
	return r.Apply(Change{Prefix: prefix, Caps: caps})
}

// Change is one prefix replacement in an Apply: every capability under
// Prefix is removed and Caps, which must all live under Prefix, installed.
// Empty Caps unloads the prefix.
type Change struct {
	Prefix string
	Caps   []Capability
}

// Apply makes several prefix replacements as one snapshot swap: either every
// change takes effect together or, when any capability is invalid, a name
// is outside its change's prefix, two changes overlap, or the result has a
// supersession cycle, none does and the registry is unchanged. A concurrent
// List never sees some changes applied and others not.
func (r *Registry) Apply(changes ...Change) error {
	norm := map[capKey]resolvedCap{}
	for i, ch := range changes {
		if err := validateName(ch.Prefix); err != nil {
			return err
		}
		for _, other := range changes[:i] {
			if underPrefix(other.Prefix, ch.Prefix) || underPrefix(ch.Prefix, other.Prefix) {
				return Errorf(Invalid, "Apply: prefixes %q and %q overlap", other.Prefix, ch.Prefix)
			}
		}
		for _, c := range ch.Caps {
			k, rc, err := normalize(c)
			if err != nil {
				return err
			}
			if !underPrefix(ch.Prefix, k.name) {
				return Errorf(Invalid, "Replace(%q): capability %s is outside the prefix", ch.Prefix, k.name)
			}
			if _, dup := norm[k]; dup {
				return Errorf(Conflict, "duplicate capability %s in Replace", refOf(k.name, k.version))
			}
			norm[k] = rc
		}
	}
	results := r.runChecksFor(norm) // host code: outside writeMu, as in Add
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.snap.Load()
	merged := make(map[capKey]resolvedCap, len(cur.caps)+len(norm))
	for k, v := range cur.caps {
		if !changed(changes, k.name) {
			merged[k] = v
		}
	}
	for k, v := range norm {
		merged[k] = v
	}
	return r.publishLocked(merged, results)
}

// changed reports whether name falls under any change's prefix.
func changed(changes []Change, name string) bool {
	for _, ch := range changes {
		if underPrefix(ch.Prefix, name) {
			return true
		}
	}
	return false
}

// Remove deletes refs: "name" removes all versions, "name@2" one version.
// Unknown or malformed refs are silently ignored.
func (r *Registry) Remove(refs ...string) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.snap.Load()
	merged := make(map[capKey]resolvedCap, len(cur.caps))
	for k, v := range cur.caps {
		merged[k] = v
	}
	changed := false
	for _, ref := range refs {
		name, ver, pinned, ok := ParseRef(ref)
		if !ok {
			continue
		}
		if pinned {
			if _, exists := merged[capKey{name, ver}]; exists {
				delete(merged, capKey{name, ver})
				changed = true
			}
		} else {
			for k := range merged {
				if k.name == name {
					delete(merged, k)
					changed = true
				}
			}
		}
	}
	if !changed {
		return
	}
	// Removing capabilities cannot create a supersession cycle.
	s, err := buildSnapshot(merged)
	if err != nil {
		return
	}
	s.groups = cur.groups
	r.pruneReadyLocked(s)
	r.snap.Store(s)
	r.notify()
}

// runChecksFor runs the readiness checks for a batch about to be added.
// It runs host code, so callers invoke it before taking writeMu.
func (r *Registry) runChecksFor(norm map[capKey]resolvedCap) map[capKey]readyState {
	results := make(map[capKey]readyState, len(norm))
	for k, rc := range norm {
		results[k] = r.runCheck(rc.capab)
	}
	return results
}

// publishLocked validates the prospective snapshot, installs the
// readiness results the caller pre-ran for the newly added capabilities
// (checks run before publishing so fresh List() results carry a real
// Ready value and Invoke can fail fast), swaps the snapshot in and
// notifies. Callers hold writeMu. Readiness is written before the
// snapshot swap, so a reader that sees a new capability always sees its
// check result too.
func (r *Registry) publishLocked(caps map[capKey]resolvedCap, results map[capKey]readyState) error {
	s, err := buildSnapshot(caps)
	if err != nil {
		return err
	}
	s.groups = r.snap.Load().groups
	r.readyMu.Lock()
	for k := range r.ready {
		if _, ok := s.caps[k]; !ok {
			delete(r.ready, k)
		}
	}
	for k, v := range results {
		if _, ok := s.caps[k]; ok {
			r.ready[k] = v
		}
	}
	r.readyMu.Unlock()
	r.snap.Store(s)
	r.notify()
	return nil
}

func (r *Registry) pruneReadyLocked(s *snapshot) {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	for k := range r.ready {
		if _, ok := s.caps[k]; !ok {
			delete(r.ready, k)
		}
	}
}
