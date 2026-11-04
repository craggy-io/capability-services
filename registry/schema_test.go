package registry_test

import (
	"encoding/json"
	"testing"

	"github.com/craggy-io/capability-services/registry"
)

// A json.RawMessage field carries any JSON value and a []byte field a
// base64 string, so their derived schemas must accept them: a client that validates results against the
// advertised output schema rejects the capability otherwise.
func TestSchemaForRawMessageAcceptsAnyJSON(t *testing.T) {
	type report struct {
		Name   string          `json:"name"`
		Report json.RawMessage `json:"report"`
		Blob   []byte          `json:"blob"`
		Extra  []struct {
			Data json.RawMessage `json:"data"`
		} `json:"extra"`
	}
	s, err := registry.SchemaFor[report]()
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []string{
		`{"name":"a","report":{"k":[1,2],"n":{"x":true}},"blob":"aGk=","extra":[{"data":{"a":1}},{"data":[1]},{"data":"s"},{"data":null}]}`,
		`{"name":"a","report":null,"blob":"","extra":[]}`,
	} {
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatal(err)
		}
		if err := res.Validate(v); err != nil {
			t.Errorf("%s: %v", doc, err)
		}
	}
}
