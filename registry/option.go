package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// Option configures either a Capability built by Func or a Registry built
// by New. Each constructor below applies to one of the two; passing a
// capability option to New (or vice versa) is a no-op.
type Option struct {
	capFn     func(*Capability)
	regFn     func(*config)
	setsInput bool
	setsOut   bool
}

type config struct {
	middleware     []Middleware
	limits         map[string]int
	checkEvery     time.Duration
	keepJobs       time.Duration
	jobAccess      func(Caller, JobStatus) bool
	validateOutput bool
	checkTimeout   time.Duration
	drainTimeout   time.Duration
	shared         SharedJobs
	dispatcher     Dispatcher
}

func defaultConfig() config {
	return config{
		limits:         map[string]int{},
		checkEvery:     30 * time.Second,
		keepJobs:       time.Hour,
		validateOutput: debugValidateOutput,
		checkTimeout:   10 * time.Second,
		drainTimeout:   10 * time.Second,
	}
}

// --- Registry options ---

// Use appends middleware. The first middleware added is outermost.
func Use(mw ...Middleware) Option {
	return Option{regFn: func(c *config) { c.middleware = append(c.middleware, mw...) }}
}

// Limit bounds concurrent calls to capabilities carrying tag, e.g.
// Limit("analysis", 4). It is application backpressure only; it reserves no
// physical resources. A limit of 0 or less is ignored.
func Limit(tag string, n int) Option {
	return Option{regFn: func(c *config) {
		if n > 0 {
			c.limits[tag] = n
		}
	}}
}

// CheckEvery sets the readiness re-check interval (default 30s). A value of
// 0 or less disables periodic re-checks; checks still run when a capability
// is added and on RefreshChecks.
func CheckEvery(d time.Duration) Option {
	return Option{regFn: func(c *config) { c.checkEvery = d }}
}

// KeepJobs sets how long finished jobs stay in the in-memory job table
// (default 1h). A value of 0 or less keeps them until Close.
func KeepJobs(d time.Duration) Option {
	return Option{regFn: func(c *config) { c.keepJobs = d }}
}

// WithJobAccess replaces the default job-access rule (a caller sees jobs
// from its own Session and from sessions it started via Parent).
func WithJobAccess(f func(Caller, JobStatus) bool) Option {
	return Option{regFn: func(c *config) { c.jobAccess = f }}
}

// ValidateOutput forces output-schema validation on or off. By default it
// is on only in builds with the craggy_debug build tag.
func ValidateOutput(on bool) Option {
	return Option{regFn: func(c *config) { c.validateOutput = on }}
}

// CheckTimeout bounds a single readiness Check call (default 10s). A
// Check that ignores its context past the bound counts as failed; its
// goroutine is abandoned (Go cannot kill it), so checks should still
// honour their context.
func CheckTimeout(d time.Duration) Option {
	return Option{regFn: func(c *config) {
		if d > 0 {
			c.checkTimeout = d
		}
	}}
}

// JobDrainTimeout bounds how long Close waits for cancelled job runners to
// return (default 10s). Handlers that honour their context return promptly;
// one that ignores cancellation past the bound is abandoned and Close
// returns without it. A value of 0 or less makes Close cancel jobs without
// waiting for them.
func JobDrainTimeout(d time.Duration) Option {
	return Option{regFn: func(c *config) { c.drainTimeout = d }}
}

// --- Capability options (for Func) ---

// Describe sets the description agents read.
func Describe(s string) Option {
	return Option{capFn: func(c *Capability) { c.Description = s }}
}

// WithExample appends a sample input agents read beside the description
// (see Example). input is any JSON-marshalable value — usually the
// capability's own typed input, so the compiler checks field names; it
// must validate against the input schema or Add fails. note is one line
// on when to call it this way; empty is allowed. WithExample panics if
// input cannot be marshalled, a programmer error like Func's.
func WithExample(note string, input any) Option {
	raw, err := json.Marshal(input)
	if err != nil {
		panic(fmt.Sprintf("registry.WithExample(%q): %v", note, err))
	}
	return Option{capFn: func(c *Capability) {
		c.Examples = append(c.Examples, Example{Note: note, Input: raw})
	}}
}

// WithVersion sets the version (default 1).
func WithVersion(v int) Option {
	return Option{capFn: func(c *Capability) { c.Version = v }}
}

// WithEffects sets the effect hints.
func WithEffects(e Effects) Option {
	return Option{capFn: func(c *Capability) { c.Effects = e }}
}

// WithTags appends tags.
func WithTags(tags ...string) Option {
	return Option{capFn: func(c *Capability) { c.Tags = append(c.Tags, tags...) }}
}

// WithSupersedes declares refs this capability replaces.
func WithSupersedes(refs ...string) Option {
	return Option{capFn: func(c *Capability) { c.Supersedes = append(c.Supersedes, refs...) }}
}

// WithAccepts appends content types this capability can run on. An empty
// list means the capability is not a content step. Type and Level are
// opaque; the host interprets them.
func WithAccepts(subjects ...Subject) Option {
	return Option{capFn: func(c *Capability) { c.Accepts = append(c.Accepts, subjects...) }}
}

// WithEmits appends a hint about content the capability's output may be.
// It is not a result and not a guarantee.
func WithEmits(subjects ...Subject) Option {
	return Option{capFn: func(c *Capability) { c.Emits = append(c.Emits, subjects...) }}
}

// AsLong marks the capability long-running: projections start it as a job
// instead of waiting. heartbeat is the stalled threshold (0 = 30s).
func AsLong(heartbeat time.Duration) Option {
	return Option{capFn: func(c *Capability) { c.Long = true; c.Heartbeat = heartbeat }}
}

// WithOrigin records where the capability's definition came from.
func WithOrigin(o Origin) Option {
	return Option{capFn: func(c *Capability) { c.Origin = o }}
}

// WithCheck sets the readiness check.
func WithCheck(f func(ctx context.Context) error) Option {
	return Option{capFn: func(c *Capability) { c.Check = f }}
}

// WithInput overrides the derived input schema. Pass nil for no input.
func WithInput(s *jsonschema.Schema) Option {
	return Option{setsInput: true, capFn: func(c *Capability) { c.Input = s }}
}

// WithOutput overrides the derived output schema. Pass nil for unstructured.
func WithOutput(s *jsonschema.Schema) Option {
	return Option{setsOut: true, capFn: func(c *Capability) { c.Output = s }}
}

func applyCapOpts(c *Capability, opts []Option) {
	for _, o := range opts {
		if o.capFn != nil {
			o.capFn(c)
		}
	}
}
