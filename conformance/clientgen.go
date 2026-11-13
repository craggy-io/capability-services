package conformance

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/craggy-io/capability-services/clientgen"
	"github.com/craggy-io/capability-services/httpapi"
	"github.com/craggy-io/capability-services/registry"
)

// The program compiled against the generated client: main.go, and how
// each set reads every page of its paged capability.
//
//go:embed testdata/clientgen
var clientDriver embed.FS

// clientDrivers generates one typed client for both capability sets
// from the view's descriptors, old versions and superseded entries
// included, and compiles a program for each set against it, in a module
// that requires nothing: the generated code may use the standard library
// alone. It returns the program for each namespace, or nil when there is
// no go tool to compile with.
func clientDrivers(t *testing.T, view *registry.View) map[string]string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		return nil
	}
	var descs []registry.Descriptor
	for _, d := range view.Descriptors(true) {
		if strings.HasPrefix(d.Name, "test.") || strings.HasPrefix(d.Name, "star.") {
			descs = append(descs, d)
		}
	}
	files, err := clientgen.Generate(descs, clientgen.Options{Package: "client", Group: clientgen.ByNamespace})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", []byte("module conformance\n\ngo 1.23\n"))
	for name, src := range files {
		write(filepath.Join("client", name), src)
	}
	source := func(name string) []byte {
		data, err := clientDriver.ReadFile("testdata/clientgen/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	write("cmd/test/main.go", source("main.go"))
	write("cmd/test/pages.go", source("pages_typed.go"))
	write("cmd/star/main.go", bytes.ReplaceAll(source("main.go"), []byte("Test"), []byte("Star")))
	write("cmd/star/pages.go", source("pages_untyped.go"))

	bin := filepath.Join(dir, "bin")
	cmd := exec.Command(goTool, "build", "-o", bin+string(filepath.Separator), "./cmd/...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the generated client: %v\n%s", err, out)
	}
	drivers := map[string]string{}
	for _, ns := range []string{"test", "star"} {
		matches, _ := filepath.Glob(filepath.Join(bin, ns+"*"))
		if len(matches) != 1 {
			t.Fatalf("built programs for %s: %v", ns, matches)
		}
		drivers[ns] = matches[0]
	}
	return drivers
}

// clientgenProjection holds the generated typed client to what Invoke
// returns: the compiled program calls every capability of the set under
// ns through the generated functions, over the HTTP projection, and each
// result must be the direct call's, each failure the same envelope.
func clientgenProjection(t *testing.T, view *registry.View, ctx context.Context, ns, driver string) {
	if driver == "" {
		t.Skip("no go tool to compile the generated client with")
	}
	srv := httptest.NewServer(httpapi.Handler(view))
	defer srv.Close()
	cmd := exec.Command(driver, srv.URL)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("the generated client's program: %v\n%s", err, stderr.String())
	}
	type outcome struct {
		Name  string          `json:"name"`
		Out   json.RawMessage `json:"out"`
		Error string          `json:"error"`
	}
	got := map[string]outcome{}
	sc := bufio.NewScanner(bytes.NewReader(output))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var o outcome
		if err := json.Unmarshal(sc.Bytes(), &o); err != nil {
			t.Fatalf("program output %q: %v", sc.Text(), err)
		}
		got[o.Name] = o
	}

	// same requires the program's outcome for a call to be what Invoke
	// gives for the same ref and input: the output, or the error envelope.
	same := func(call, ref, in string) {
		t.Helper()
		o, ok := got[call]
		if !ok {
			t.Fatalf("%s: the program did not report it", call)
		}
		want, err := view.Invoke(ctx, ns+"."+ref, json.RawMessage(in))
		if err != nil {
			env, _ := json.Marshal(registry.ErrorEnvelope{Error: registry.Info(err)})
			if canonical([]byte(o.Error)) != canonical(env) {
				t.Fatalf("%s: client error %q, Invoke %s", call, o.Error, env)
			}
			return
		}
		if o.Error != "" || canonical(o.Out) != canonical(want) {
			t.Fatalf("%s: client %s (error %q), Invoke %s", call, o.Out, o.Error, want)
		}
	}
	const hello = `{"text":"hello"}`
	same("echo", "echo", hello)
	same("add", "add", `{"a":19,"b":23}`)
	same("fail", "fail", hello)
	same("deny", "deny", hello)
	same("full", "full", hello)
	same("admin", "admin", hello)
	same("old", "old", hello) // superseded entries still answer
	same("new", "new", hello)
	same("ver@1", "ver@1", hello) // each version generated calls its own
	same("ver@2", "ver@2", hello)
	same("unready", "unready", hello)
	same("slow", "slow", `{"label":"clientgen"}`) // a Long call returns its result, not a handle
	same("items.list", "items.list", `{"limit":3}`)
	if canonical(got["echo"].Out) != `{"text":"hello"}` || canonical(got["add"].Out) != `{"sum":42}` ||
		!strings.Contains(string(got["ver@1"].Out), "v1:hello") || !strings.Contains(string(got["slow"].Out), "done-clientgen") {
		t.Fatalf("outcomes = %+v", got)
	}
	// A script's own failure is internal, so only the Go set keeps the reason.
	if (ns == "test" && !strings.Contains(got["full"].Error, `"reason":"no_capacity"`)) || !strings.Contains(got["unready"].Error, `"code":"unavailable"`) {
		t.Fatalf("full = %q, unready = %q", got["full"].Error, got["unready"].Error)
	}
	// Following the cursor visits each item exactly once.
	var items []string
	if err := json.Unmarshal(got["items.all"].Out, &items); err != nil || !slices.Equal(items, Items) {
		t.Fatalf("items.all = %s (error %q), want %v", got["items.all"].Out, got["items.all"].Error, Items)
	}
}
