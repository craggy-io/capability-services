package httpapi_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/craggy-io/capability-services/httpapi"
	"github.com/craggy-io/capability-services/registry"
)

type getIn struct {
	ID string `json:"id"`
}

// The HTTP projection: POST /v1/{ref as path}, errors as the shared
// envelope mapped to a status, 202 + Location for Long capabilities, and
// the Craggy-Superseded-By header on superseded ones. Authentication is the
// host's: here a Caller option derives the caller from a header.
func ExampleHandler() {
	r := registry.New()
	defer r.Close()
	get := registry.Func("storage.artifact.get", "Get an artifact",
		func(ctx context.Context, in getIn) (map[string]string, error) {
			if in.ID != "art-1" {
				return nil, registry.Errorf(registry.NotFound, "artifact %q not found", in.ID)
			}
			return map[string]string{"id": in.ID}, nil
		})
	legacy := registry.Func("jadx.decompile", "Decompile (legacy)",
		func(ctx context.Context, in getIn) (string, error) { return "jadx", nil })
	modern := registry.Func("apk.decompile", "Decompile",
		func(ctx context.Context, in getIn) (string, error) { return "apk", nil },
		registry.AsLong(0), registry.WithSupersedes("jadx.decompile"))
	if err := r.Add(get, legacy, modern); err != nil {
		panic(err)
	}
	h := httpapi.Handler(r.View(registry.All), httpapi.Caller(func(req *http.Request) registry.Caller {
		return registry.Caller{ID: "user:" + req.Header.Get("X-User"), Kind: "user"}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	post := func(path, body string) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		fmt.Println(resp.StatusCode, strings.TrimSpace(string(b)))
		if s := resp.Header.Get("Craggy-Superseded-By"); s != "" {
			fmt.Println("  Craggy-Superseded-By:", s)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			fmt.Println("  Location starts with /v1/jobs/:", strings.HasPrefix(loc, "/v1/jobs/"))
		}
	}
	post("/v1/storage/artifact/get", `{"id":"art-1"}`)
	post("/v1/storage/artifact/get@1", `{"id":"art-2"}`)
	post("/v1/storage/artifact/get", `{"id":7}`)
	post("/v1/jadx/decompile", `{"id":"art-1"}`)

	resp, err := http.Post(srv.URL+"/v1/apk/decompile", "application/json", strings.NewReader(`{"id":"art-1"}`))
	if err != nil {
		panic(err)
	}
	resp.Body.Close()
	fmt.Println(resp.StatusCode, strings.HasPrefix(resp.Header.Get("Location"), "/v1/jobs/"))
	// Output:
	// 200 {"result":{"id":"art-1"}}
	// 404 {"error":{"code":"not_found","message":"artifact \"art-2\" not found","retryable":false}}
	// 400 {"error":{"code":"invalid","message":"storage.artifact.get@1: invalid input:\n- id: wrong type: expected string, got integer 7\nRequired fields: id.","retryable":false}}
	// 200 {"result":"jadx"}
	//   Craggy-Superseded-By: apk.decompile
	// 202 true
}
