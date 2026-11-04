package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

var (
	rawMessageType = reflect.TypeFor[json.RawMessage]()
	bytesType      = reflect.TypeFor[[]byte]()
	emptyStruct    = reflect.TypeFor[struct{}]()
)

// SchemaFor derives a JSON Schema from a Go type (json tags, plus
// `jsonschema:"..."` field descriptions). Byte slices are described as
// the JSON they marshal to, not as the Go type they are: a json.RawMessage
// field is any JSON value and a []byte field a base64 string. Left alone,
// the deriver describes both as an array of integers, and a client
// validating results against the advertised schema rejects every object
// or string such a field carries.
func SchemaFor[T any]() (*jsonschema.Schema, error) {
	return jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		rawMessageType: {},
		bytesType:      {Type: "string", ContentEncoding: "base64"},
	}})
}

// ParseSchema parses a JSON Schema document. "true" and "false" (boolean
// schemas) are accepted: true becomes an empty schema, false one that
// matches nothing.
func ParseSchema(data json.RawMessage) (*jsonschema.Schema, error) {
	switch string(data) {
	case "true":
		return &jsonschema.Schema{}, nil
	case "false":
		return &jsonschema.Schema{Not: &jsonschema.Schema{}}, nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, WrapCode(Invalid, err)
	}
	// Reject documents that don't resolve (bad $ref, bad pattern, ...).
	if _, err := s.Resolve(nil); err != nil {
		return nil, WrapCode(Invalid, err)
	}
	return &s, nil
}

// EmptyObjectSchema is the schema projections present for a capability with
// no input: an object with no properties. It returns a fresh copy.
func EmptyObjectSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

func resolveSchema(s *jsonschema.Schema) (*jsonschema.Resolved, error) {
	if s == nil {
		return nil, nil
	}
	return s.Resolve(nil)
}

func validateAgainst(res *jsonschema.Resolved, raw json.RawMessage) error {
	if res == nil {
		return nil
	}
	var inst any
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&inst); err != nil {
		return WrapCode(Invalid, err)
	}
	inst = normalizeNumber(inst)
	if err := res.Validate(inst); err != nil {
		return WrapCode(Invalid, err)
	}
	return nil
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// normalizeNumber converts json.Number leaves to int64/float64 so schema
// numeric checks see plain Go numbers.
func normalizeNumber(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeNumber(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeNumber(e)
		}
		return t
	default:
		return v
	}
}

// Func builds a Capability from a typed Go function. The schemas are derived
// from In and Out (json tags, plus `jsonschema:"..."` descriptions) unless
// overridden with WithInput/WithOutput. In or Out may be json.RawMessage
// (passed through untouched, no derived schema) or any (unstructured).
//
// Func panics on programmer errors: a nil function, or a type whose schema
// cannot be derived. Name/schema/supersession validation that depends on
// registry state happens in Add, which returns an error.
func Func[In, Out any](name, title string, fn func(context.Context, In) (Out, error), opts ...Option) Capability {
	if fn == nil {
		panic(fmt.Sprintf("registry.Func(%s): nil function", name))
	}
	c := Capability{Name: name, Title: title, Version: 1}

	setsIn, setsOut := false, false
	for _, o := range opts {
		setsIn = setsIn || o.setsInput
		setsOut = setsOut || o.setsOut
	}
	inT, outT := reflect.TypeFor[In](), reflect.TypeFor[Out]()
	rawIn := inT == rawMessageType
	rawOut := outT == rawMessageType
	anyIn := inT == nil || inT.Kind() == reflect.Interface
	anyOut := outT == nil || outT.Kind() == reflect.Interface

	if !setsIn {
		switch {
		case rawIn, anyIn:
			c.Input = &jsonschema.Schema{}
		case inT == emptyStruct:
			c.Input = EmptyObjectSchema()
		default:
			s, err := SchemaFor[In]()
			if err != nil {
				panic(fmt.Sprintf("registry.Func(%s): deriving input schema: %v", name, err))
			}
			c.Input = s
		}
	}
	if !setsOut && !rawOut && !anyOut {
		s, err := SchemaFor[Out]()
		if err != nil {
			panic(fmt.Sprintf("registry.Func(%s): deriving output schema: %v", name, err))
		}
		c.Output = s
	}

	c.Handler = func(ctx context.Context, raw json.RawMessage) (any, error) {
		var in In
		switch {
		case rawIn:
			// In is json.RawMessage; assign via reflection-free trick:
			// the only way inT matches is In == json.RawMessage.
			*any(&in).(*json.RawMessage) = append(json.RawMessage(nil), raw...)
		case len(raw) == 0 || string(raw) == "null":
			// leave zero value
		default:
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, WrapCode(Invalid, err)
			}
		}
		out, err := fn(ctx, in)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	applyCapOpts(&c, opts)
	return c
}
