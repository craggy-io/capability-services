package clientgen

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/craggy-io/capability-services/registry"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// hostkitRefs are the capabilities the hand-written client package of the
// Craggy marketplace bundles (lib/hostkit) wraps, as far as the two
// providers describe them.
var hostkitRefs = []string{
	"artifacts.content@1", "artifacts.files@1", "artifacts.file.read@1", "artifacts.get@1",
	"artifacts.rows@1", "artifacts.runs.record@1", "artifacts.runs.record-artifacts@1",
	"artifacts.runs.record-tree@1", "artifacts.runs.find@1",
	"workers.do@1", "workers.exec@1", "workers.ready@1",
}

// descriptors reads descriptor files under testdata.
func descriptors(t *testing.T, names ...string) []registry.Descriptor {
	t.Helper()
	var all []registry.Descriptor
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		descs, err := ParseDescriptors(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		all = append(all, descs...)
	}
	return all
}

// smallClient is the client for the hand-written descriptor set.
func smallClient(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := Generate(descriptors(t, "small.json"), Options{Package: "small", Group: ByNamespace})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// hostkitClient is the client for what hostkit wraps, from the describe
// replies of the two subsystems that provide it.
func hostkitClient(t *testing.T) map[string][]byte {
	t.Helper()
	descs := descriptors(t, "describe/artifacts.json", "describe/workers.json")
	files, err := Generate(descs, Options{Package: "hostclient", Include: hostkitRefs, Group: ByNamespace})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The generated files for a small descriptor set, and for the real
// descriptors hostkit's callers need, are the golden files. Run
// go test ./clientgen -update after a deliberate change.
func TestGolden(t *testing.T) {
	for set, files := range map[string]map[string][]byte{"small": smallClient(t), "hostclient": hostkitClient(t)} {
		dir := filepath.Join("testdata", "golden", set)
		if *update {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, dir, files)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var have, want []string
		for _, e := range entries {
			have = append(have, e.Name())
		}
		for name, src := range files {
			want = append(want, name)
			golden, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Errorf("%s: %v", set, err)
				continue
			}
			// A checkout may give the golden files the system's line ends.
			if !bytes.Equal(bytes.ReplaceAll(golden, []byte("\r\n"), []byte("\n")), src) {
				t.Errorf("%s/%s differs from its golden file; run go test ./clientgen -update and review the change", set, name)
			}
		}
		sort.Strings(want)
		if len(have) != len(want) {
			t.Errorf("%s: golden files %v, generated %v", set, have, want)
		}
	}
}

// Every real descriptor of the two providers generates, and so does the
// whole set in one package.
func TestEveryProviderDescriptorGenerates(t *testing.T) {
	descs := descriptors(t, "describe/artifacts.json", "describe/workers.json")
	if len(descs) < 40 {
		t.Fatalf("only %d descriptors in the describe replies", len(descs))
	}
	if _, err := Generate(descs, Options{Package: "all", Group: ByNamespace}); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The generated clients compile in a module that requires nothing, pass
// go vet, and behave: testdata/behaviour holds tests that are compiled
// with them and run there. small_test.go checks every mapping rule on
// values (the JSON a generated type marshals to against a hand-written
// struct with the same tags, unknown members, numbers, paging against a
// fake Caller). hostkit_test.go checks the client for the real
// descriptors against copies of hostkit's hand-written types and of its
// page-reading loops.
func TestGeneratedCodeBehaves(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool to compile the generated code with")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module generated\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for pkg, files := range map[string]map[string][]byte{"small": smallClient(t), "hostclient": hostkitClient(t)} {
		writeFiles(t, filepath.Join(dir, pkg), files)
	}
	for from, to := range map[string]string{"small_test.go": "small", "hostkit_test.go": "hostclient"} {
		src, err := os.ReadFile(filepath.Join("testdata", "behaviour", from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, to, from), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"vet", "./..."}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command(goTool, args...)
		cmd.Dir = dir
		// No workspace and no proxy: the module must build from the
		// standard library alone.
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %v in the generated module: %v\n%s", args, err, out)
		}
		t.Logf("go %v\n%s", args, out)
	}
}
