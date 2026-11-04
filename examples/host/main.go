// Command host is a runnable example that wires everything together with
// fake domains: an in-memory artifact store, a toy classifier, a legacy
// "jadx" capability superseded at startup by a Starlark-defined
// apk.decompile, the job capabilities, an HTTP API (default) and an MCP
// server (-mcp, stdio).
//
//	go run ./examples/host                  # HTTP on :8080
//	go run ./examples/host -addr :9000
//	go run ./examples/host -mcp              # MCP over stdio
//	go run ./examples/host -scripts ./caps   # also hot-reload ./caps/*.star
//
// The startup listing goes to stdout in HTTP mode and to stderr with -mcp,
// where stdout carries only the JSON-RPC stream; logs go to stderr.
//
// With -scripts, every top-level *.star file in DIR defines the
// capabilities under its stem (caps/apk.star defines apk.*), modules load
// from DIR (opened as an os.Root: symbolic links cannot reach outside it),
// a DIR/types.go declares types for every file, and edits are picked up
// within a second (see starlarkcap.Watch). A file named apk.star replaces
// the built-in demo.
//
// Try:
//
//	curl localhost:8080/v1/
//	curl -XPOST localhost:8080/v1/storage/artifact/get -d '{"id":"art-1"}'
//	curl -XPOST localhost:8080/v1/apk/decompile -d '{"artifact":"art-1"}'   # 202 + Location
//	curl localhost:8080/v1/jobs/<id>/events
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/craggy-io/capability-services/httpapi"
	"github.com/craggy-io/capability-services/jobs"
	regmcp "github.com/craggy-io/capability-services/mcp"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

// --- fake domain: an artifact store that knows nothing of the registry ---

type artifact struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}

type store struct {
	mu    sync.RWMutex
	byID  map[string]artifact
	order []string
}

func newStore() *store {
	s := &store{byID: map[string]artifact{}}
	for _, a := range []artifact{
		{"art-1", "app.apk", "apk", "sha256:aaa"},
		{"art-2", "libfoo.so", "elf", "sha256:bbb"},
		{"art-3", "notes.txt", "text", "sha256:ccc"},
	} {
		s.byID[a.ID] = a
		s.order = append(s.order, a.ID)
	}
	return s
}

// The Starlark file loaded at startup. It supersedes the Go-registered
// legacy jadx capability for every projection, with no extra wiring.
const apkStar = `
def _decompile(ctx, input):
    ctx.progress(stage = "pull-image", done = 1, total = 1, unit = "steps")
    art = ctx.call("storage.artifact.get", id = input["artifact"])
    ctx.progress(stage = "decompile", done = 50, total = 100, unit = "files", data = {"tool": "apk.star"})
    ctx.progress(stage = "decompile", done = 100, total = 100, unit = "files")
    ctx.progress(stage = "publish-output")
    ctx.progress(stage = "finalise")
    return {"artifact": art["name"], "sources": ["Main.java", "Util.java"], "via": "apk.star"}

capability(
    name = "apk.decompile",
    title = "Decompile an APK (script)",
    description = "Decompile an APK to sources and resources in an isolated container",
    supersedes = ["jadx.decompile"],
    long = True,
    effects = ["open_world"],
    tags = ["agent", "analysis"],
    input = {"artifact": schema.string(required = True, description = "artifact id")},
    run = _decompile,
)
`

type getIn struct {
	ID string `json:"id" jsonschema:"artifact id"`
}
type classifyIn struct {
	Hex string `json:"hex" jsonschema:"up to 16 bytes as hex"`
}
type classifyOut struct {
	Type string `json:"type"`
}
type listIn struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

func capabilities(s *store) []registry.Capability {
	return []registry.Capability{
		registry.Func("storage.artifact.get", "Get an artifact's metadata",
			func(ctx context.Context, in getIn) (artifact, error) {
				s.mu.RLock()
				defer s.mu.RUnlock()
				a, ok := s.byID[in.ID]
				if !ok {
					return artifact{}, registry.Errorf(registry.NotFound, "artifact %q not found", in.ID)
				}
				return a, nil
			},
			registry.Describe("Look up one stored artifact by id."),
			registry.WithEffects(registry.ReadOnly|registry.Idempotent), registry.WithTags("agent")),
		registry.Func("storage.artifact.list", "List artifacts",
			func(ctx context.Context, in listIn) (registry.Page[artifact], error) {
				s.mu.RLock()
				defer s.mu.RUnlock()
				arts := make([]artifact, 0, len(s.order))
				for _, id := range s.order {
					arts = append(arts, s.byID[id])
				}
				return registry.Paginate(arts, registry.PageIn{Limit: in.Limit, Cursor: in.Cursor})
			},
			registry.WithEffects(registry.ReadOnly), registry.WithTags("agent")),
		registry.Func("classify.bytes", "Classify bytes by magic",
			func(ctx context.Context, in classifyIn) (classifyOut, error) {
				h := strings.ToLower(strings.TrimPrefix(in.Hex, "0x"))
				switch {
				case strings.HasPrefix(h, "504b"):
					return classifyOut{"zip/apk"}, nil
				case strings.HasPrefix(h, "7f454c46"):
					return classifyOut{"elf"}, nil
				default:
					return classifyOut{"unknown"}, nil
				}
			},
			registry.WithEffects(registry.ReadOnly), registry.WithTags("agent")),
		// Legacy capability the Starlark file supersedes at startup.
		registry.Func("jadx.decompile", "Decompile with jadx (legacy)",
			func(ctx context.Context, in getIn) (map[string]string, error) {
				registry.Progress(ctx, registry.Update{Stage: "decompile", Done: 1, Total: 1, Unit: "files"})
				return map[string]string{"artifact": in.ID, "via": "jadx"}, nil
			},
			registry.AsLong(time.Second), registry.WithTags("agent", "analysis")),
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

// nopCloser lets the MCP transport own stdout without closing it.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// run is main with its arguments and standard streams passed in, so tests
// can check what goes where.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("host", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", ":8080", "HTTP listen address")
	useMCP := flags.Bool("mcp", false, "serve MCP over stdio instead of HTTP")
	scripts := flags.String("scripts", "", "directory of capability *.star files to load and hot-reload")
	if err := flags.Parse(args); err != nil {
		return err
	}
	log := log.New(stderr, "", log.LstdFlags)
	// SIGINT/SIGTERM cancel ctx: the servers stop accepting, and the
	// deferred closes below (MCP stop, Watcher.Close, Registry.Close,
	// in that order) drain jobs instead of the process dying mid-job.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Example middleware: one audit line per call (in the host application this
	// writes a storage Run; here it just logs). Note it records the exact
	// name@version that ran.
	audit := func(next registry.Handler, c registry.Capability) registry.Handler {
		return func(ctx context.Context, in json.RawMessage) (any, error) {
			caller, _ := registry.CallerFrom(ctx)
			jobID, _ := registry.JobIDFrom(ctx)
			start := time.Now()
			out, err := next(ctx, in)
			log.Printf("audit %s job=%s caller=%q session=%q err=%v in %s", c.Ref(), jobID, caller.ID, caller.Session, err, time.Since(start).Round(time.Millisecond))
			return out, err
		}
	}

	r := registry.New(registry.Use(audit), registry.Limit("analysis", 2))
	defer r.Close()
	st := newStore()
	caps := append(capabilities(st), jobs.Capabilities()...)
	if err := r.Add(caps...); err != nil {
		return err
	}
	if err := r.AddGroup(jobs.Group()); err != nil {
		return err
	}
	// Supersession policy for script files: a file may always supersede
	// capabilities under its own prefix; outside it, only what this filter
	// admits. Here scripts may take over the legacy jadx.* tools, and
	// nothing else (storage, classify, jobs) can be shadowed by a script.
	loadOpts := starlarkcap.LoadOptions{
		MaySupersede: registry.Match("jadx.*"),
		Print: func(pos starlarkcap.Position, msg string) {
			log.Printf("%s:%d: %s", pos.File, pos.Line, msg)
		},
	}
	l, err := starlarkcap.Load(context.Background(), r, "apk", starlarkcap.Source{Filename: "apk.star", Src: []byte(apkStar)}, loadOpts)
	if err != nil {
		return err
	}
	log.Printf("loaded apk.star (%s): %s", l.Digest, strings.Join(l.Capabilities, ", "))
	if *scripts != "" {
		w, err := starlarkcap.Watch(ctx, r, starlarkcap.WatchOptions{
			Dir:  *scripts, // opened as an os.Root: no symlink escapes
			Load: loadOpts,
			OnResult: func(prefix string, l starlarkcap.Loaded, err error) {
				switch {
				case err != nil:
					log.Printf("scripts: %s: %v", prefix, err)
				case l.Digest == "": // the file was removed
					log.Printf("scripts: %s: unloaded", prefix)
				default:
					log.Printf("scripts: %s: loaded %s (%s)", prefix, strings.Join(l.Capabilities, ", "), l.Digest)
				}
			},
		})
		if err != nil {
			return err
		}
		defer w.Close()
	}
	// With -mcp, stdout is the JSON-RPC stream: list to stderr instead.
	listing := stdout
	if *useMCP {
		listing = stderr
	}
	fmt.Fprintln(listing, "registered (List):")
	for _, e := range r.List() {
		fmt.Fprintf(listing, "  %-28s %s\n", e.Capability.Ref(), e.Title)
	}
	fmt.Fprintln(listing, "groups:")
	for _, g := range r.Groups() {
		fmt.Fprintf(listing, "  %-28s %s\n", g.Name, g.Description)
	}
	fmt.Fprintln(listing, "hidden from List (superseded, still callable):")
	for _, e := range r.All() {
		if e.Superseded() {
			fmt.Fprintf(listing, "  %-28s superseded by %s\n", e.Capability.Ref(), strings.Join(e.SupersededBy, ", "))
		}
	}

	if *useMCP {
		agents := r.View(registry.And(
			registry.Or(registry.Tagged("agent"), registry.Match("jobs.*")),
			registry.Not(registry.Tagged("admin")),
		))
		server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "craggy-example", Version: "1"}, nil)
		stop := regmcp.Serve(server, agents, regmcp.OnClose(func(c registry.Caller) {
			log.Printf("mcp session closed: %s (%s)", c.ID, c.Session)
		}))
		defer stop()
		log.Printf("serving MCP on stdio (agent view: %d tools)", len(agents.List()))
		transport := &mcpsdk.IOTransport{Reader: io.NopCloser(stdin), Writer: nopCloser{stdout}}
		if err := server.Run(ctx, transport); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}

	api := r.View(registry.All)
	h := httpapi.Handler(api, httpapi.Caller(func(req *http.Request) registry.Caller {
		if u := req.Header.Get("X-User"); u != "" {
			return registry.Caller{ID: "user:" + u, Kind: "user"}
		}
		return registry.Caller{ID: "user:anonymous", Kind: "user"}
	}))
	srv := &http.Server{Addr: *addr, Handler: h}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("serving HTTP on %s — open http://localhost%s/v1/ and /v1/openapi.json", *addr, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
