// The program the conformance suite compiles against the generated typed
// client (package clientgen) and runs: it calls every capability of the
// standard set through the generated functions and prints what each
// returned, one JSON line a call. Its Caller is an HTTP client of the
// HTTP projection, written with the standard library alone, as the
// generated client is.
//
// It is written for the Go-registered set (test.*). The suite makes the
// one for the Starlark set by replacing Test with Star in the names. It
// is under testdata so that the go tool never builds it where it lies.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"conformance/client"
)

// httpCaller is a client.Caller over the HTTP projection.
type httpCaller struct{ base string }

// callError is a failed call: the error envelope the projection sent.
type callError struct{ envelope string }

func (e *callError) Error() string { return e.envelope }

func (h httpCaller) Call(ctx context.Context, ref string, in any) (json.RawMessage, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	name, version, _ := strings.Cut(ref, "@")
	url := h.base + "/v1/" + strings.ReplaceAll(name, ".", "/") + "@" + version
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// A Caller waits for a long-running capability to finish.
	req.Header.Set("Prefer", "wait=60")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &callError{envelope: strings.TrimSpace(string(data))}
	}
	var result struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Result, nil
}

// report prints one call's outcome: its output as JSON, or its error.
func report[T any](name string, out T, err error) {
	line := map[string]any{"name": name}
	if err != nil {
		line["error"] = err.Error()
	} else {
		line["out"] = out
	}
	raw, err := json.Marshal(line)
	if err != nil {
		raw = fmt.Appendf(nil, `{"name":%q,"error":%q}`, name, err.Error())
	}
	fmt.Println(string(raw))
}

func main() {
	ctx := context.Background()
	c := httpCaller{base: os.Args[1]}
	var out any
	var err error

	out, err = client.TestEcho(ctx, c, client.TestEchoIn{Text: "hello"})
	report("echo", out, err)
	out, err = client.TestAdd(ctx, c, client.TestAddIn{A: 19, B: 23})
	report("add", out, err)
	out, err = client.TestFail(ctx, c, client.TestFailIn{Text: "hello"})
	report("fail", out, err)
	out, err = client.TestDeny(ctx, c, client.TestDenyIn{Text: "hello"})
	report("deny", out, err)
	out, err = client.TestFull(ctx, c, client.TestFullIn{Text: "hello"})
	report("full", out, err)
	out, err = client.TestAdmin(ctx, c, client.TestAdminIn{Text: "hello"})
	report("admin", out, err)
	out, err = client.TestOld(ctx, c, client.TestOldIn{Text: "hello"})
	report("old", out, err)
	out, err = client.TestNew(ctx, c, client.TestNewIn{Text: "hello"})
	report("new", out, err)
	// Both versions were generated, so each carries its version.
	out, err = client.TestVerV1(ctx, c, client.TestVerV1In{Text: "hello"})
	report("ver@1", out, err)
	out, err = client.TestVerV2(ctx, c, client.TestVerV2In{Text: "hello"})
	report("ver@2", out, err)
	out, err = client.TestUnready(ctx, c, client.TestUnreadyIn{Text: "hello"})
	report("unready", out, err)
	// Long-running: the function returns the result, not a job handle.
	out, err = client.TestSlow(ctx, c, client.TestSlowIn{Label: "clientgen"})
	report("slow", out, err)
	out, err = client.TestItemsList(ctx, c, client.TestItemsListIn{Limit: 3})
	report("items.list", out, err)
	out, err = allItems(ctx, c)
	report("items.all", out, err)
}
