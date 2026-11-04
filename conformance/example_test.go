package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/craggy-io/capability-services/conformance"
	"github.com/craggy-io/capability-services/registry"
)

// The standard set runs twice: Go-registered (test.*) and Starlark-defined
// (star.*). Results match; a failure inside a script surfaces as internal
// with the nested capability's code in the message.
func ExampleLoadScript() {
	r, err := conformance.NewRegistry()
	if err != nil {
		panic(err)
	}
	defer r.Close()
	loaded, err := conformance.LoadScript(context.Background(), r)
	if err != nil {
		panic(err)
	}
	fmt.Println(strings.Join(loaded.Capabilities, " "))

	ctx := context.Background()
	in := json.RawMessage(`{"a":19,"b":23}`)
	goOut, _ := r.Invoke(ctx, "test.add", in)
	starOut, _ := r.Invoke(ctx, "star.add", in)
	fmt.Println(string(goOut), string(starOut))

	_, goErr := r.Invoke(ctx, "test.fail", json.RawMessage(`{"text":"x"}`))
	_, starErr := r.Invoke(ctx, "star.fail", json.RawMessage(`{"text":"x"}`))
	fmt.Println(registry.CodeOf(goErr), registry.CodeOf(starErr), strings.Contains(starErr.Error(), "conflict: boom"))
	// Output:
	// star.echo@1 star.add@1 star.fail@1 star.deny@1 star.full@1 star.admin@1 star.old@1 star.new@1 star.ver@1 star.ver@2 star.items.list@1 star.slow@1 star.unready@1
	// {"sum":42} {"sum":42}
	// conflict internal true
}
