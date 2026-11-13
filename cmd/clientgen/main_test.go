package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/capability-services/clientgen"
)

const listing = `{"capabilities":[
	{"name":"blobs.read","version":1,"input":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
	{"name":"tools.run","version":1}]}`

// The command writes what the package generates, reads a file or
// standard input, and removes only files an earlier run generated.
func TestRunWritesTheGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "client")
	file := filepath.Join(dir, "listing.json")
	if err := os.WriteFile(file, []byte(listing), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-package", "host", "-out", out, "-by-namespace", file}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	for _, name := range []string{"client.go", "blobs_client.go", "tools_client.go"} {
		src, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || !strings.HasPrefix(string(src), clientgen.Header+"\n\npackage host\n") {
			t.Fatalf("%s: %v\n%s", name, err, src)
		}
	}
	// A file of the caller's own in the same folder is left alone.
	own := filepath.Join(out, "own.go")
	if err := os.WriteFile(own, []byte("package host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// From standard input, one capability, one file: the namespace files go.
	stdout.Reset()
	if err := run([]string{"-package", "host", "-out", out, "-include", "blobs.read", "-"}, strings.NewReader(listing), &stdout, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	src, err := os.ReadFile(filepath.Join(out, "client.go"))
	if err != nil || !strings.Contains(string(src), "func BlobsRead(") || strings.Contains(string(src), "ToolsRun") {
		t.Fatalf("client.go: %v\n%s", err, src)
	}
	for _, gone := range []string{"blobs_client.go", "tools_client.go"} {
		if _, err := os.Stat(filepath.Join(out, gone)); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("the caller's own file was removed: %v", err)
	}
	if !strings.Contains(stdout.String(), "removed") || !strings.Contains(stdout.String(), "wrote") {
		t.Errorf("stdout = %s", stdout.String())
	}
}

func TestRunRefusesWhatItCannotGenerate(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "listing.json")
	if err := os.WriteFile(file, []byte(listing), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	for name, args := range map[string][]string{
		"no package":       {"-out", dir, file},
		"no file":          {"-package", "host", "-out", dir},
		"unknown ref":      {"-package", "host", "-out", filepath.Join(dir, "x"), "-include", "blobs.write@1", file},
		"not a descriptor": {"-package", "host", "-out", dir, filepath.Join(dir, "missing.json")},
	} {
		if err := run(args, nil, &stdout, &stderr); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A refused run writes nothing.
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Errorf("a refused run made its output folder (%v)", err)
	}
}
