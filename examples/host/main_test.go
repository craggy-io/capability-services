package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// In MCP mode stdout is the JSON-RPC stream: the listing goes to stderr.
func TestMCPModeKeepsStdoutForJSONRPC(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr syncBuffer
	done := make(chan error, 1)
	go func() {
		err := run([]string{"-mcp"}, inR, outW, &stderr)
		outW.Close()
		done <- err
	}()
	go func() {
		_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`+"\n")
	}()
	lines := bufio.NewScanner(outR)
	if !lines.Scan() {
		t.Fatalf("no output on stdout; stderr:\n%s", stderr.String())
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(lines.Bytes(), &msg); err != nil || msg.JSONRPC != "2.0" || msg.ID != 1 || msg.Result == nil {
		t.Fatalf("first stdout line is not the initialize response: %q", lines.Text())
	}
	inW.Close()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP server did not stop at end of input")
	}
	if !strings.Contains(stderr.String(), "registered (List):") || !strings.Contains(stderr.String(), "apk.decompile@1") {
		t.Fatalf("listing missing from stderr:\n%s", stderr.String())
	}
	// The jobs group is registered (AddGroup), so it is listed too.
	if !strings.Contains(stderr.String(), "groups:") || !strings.Contains(stderr.String(), "jobs") {
		t.Fatalf("jobs group missing from stderr listing:\n%s", stderr.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The curl lines in the header comment name artifacts the store holds.
func TestHeaderCurlUsesStoredArtifacts(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	ids := regexp.MustCompile(`"(?:id|artifact)":"([^"]+)"`).FindAllStringSubmatch(f.Doc.Text(), -1)
	if len(ids) == 0 {
		t.Fatal("no curl examples with ids in the header comment")
	}
	s := newStore()
	for _, m := range ids {
		if _, ok := s.byID[m[1]]; !ok {
			t.Fatalf("header curl uses %q, which is not an artifact id", m[1])
		}
	}
}

// -scripts takes a bundle directory: its bundle.yaml declares the capability
// files, and each is installed under its contribution id.
func TestScriptsFlagLoadsABundle(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: host.test, version: 1.0.0}\ncontributions:\n  - {id: scan, kind: capability.script@1, path: scan.star}\n",
		"scan.star":   `capability(name = "scan.hello", run = lambda ctx, i: {"hi": True})` + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stderr syncBuffer
	if err := run([]string{"-mcp", "-scripts", dir}, strings.NewReader(""), io.Discard, &stderr); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "scripts: scan: loaded scan.hello@1") || !strings.Contains(stderr.String(), "scan.hello@1") {
		t.Fatalf("scan.star not loaded:\n%s", stderr.String())
	}
}
