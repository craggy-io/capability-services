package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"regexp"
	"strconv"
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

// The example host reaches script-services only through starlarkcap.
func TestHostImportsNoScriptServices(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if strings.HasPrefix(p, "github.com/craggy-io/script-services") {
			t.Fatalf("examples/host imports %s", p)
		}
	}
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
