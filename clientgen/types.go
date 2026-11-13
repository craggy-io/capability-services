package clientgen

import (
	"encoding/json"
	"fmt"
	"strings"
)

// kind is what a Go type generated from a schema is, as far as the
// generator needs to know.
type kind int

const (
	kindRaw    kind = iota // json.RawMessage: any JSON
	kindString             // string
	kindEnum               // a named string type with constants
	kindBytes              // []byte
	kindInt                // int64
	kindNumber             // float64
	kindBool               // bool
	kindSlice              // []elem
	kindMap                // map[string]elem, or map[string]any
	kindStruct             // a named struct
)

// goType is the Go type of one schema.
type goType struct {
	expr    string // the type as Go source
	kind    kind
	elem    string // kindSlice: the element type as Go source
	pointer bool   // expr is a pointer: the schema admits null and the type has no nil of its own
	// recursive is set for a struct that is still being generated when it
	// is referred to: a field of that type has to be a pointer.
	recursive bool
}

// field is one field of a generated struct.
type field struct {
	goName   string
	jsonName string
	typ      goType
	doc      string
	optional bool
}

// decl is one generated type declaration. Declarations are kept in the
// order their types are first met, an outer type before the types of its
// fields.
type decl struct {
	text string
	json bool // a type in it is json.RawMessage: the file imports encoding/json
}

// root is one input or output schema, with the state of the named types
// made from its $defs.
type root struct {
	name   string // the Go name of the root type: <Capability>In or <Capability>Out
	ref    string // the capability, name@version
	path   string // "input" or "output"
	schema *schema
	defs   map[string]*defState
}

// at names a place in the root's schema for an error: path is "input",
// "output.files[]", "input#/$defs/Node".
func (rt *root) at(path string) string { return rt.ref + " " + path }

type defState struct {
	done bool
	typ  goType
	name string // the Go name, when the definition is a struct or an enum
}

// declare claims a Go name for origin. A name derived twice is an error:
// renaming one of the two would make the generated names unpredictable.
func (g *generator) declare(name, origin string) error {
	if name == "" {
		return fmt.Errorf("clientgen: %s has no letter or digit to make a Go name from", origin)
	}
	if reserved[name] {
		return fmt.Errorf("clientgen: %s would be named %s, which the generated package declares itself", origin, name)
	}
	if other, taken := g.names[name]; taken {
		return fmt.Errorf("clientgen: %s and %s would both be named %s", other, origin, name)
	}
	g.names[name] = origin
	return nil
}

// reserved are the exported names every generated package declares.
var reserved = map[string]bool{"Caller": true}

// typeOf maps one schema to a Go type. name is the Go name the type gets
// if it needs one (a struct or an enum); path says where the schema is
// in its root, for comments and errors. direct is true when the type will be used as a
// struct field's own type, not inside a slice or a map.
func (g *generator) typeOf(rt *root, s *schema, name, path string, direct bool) (goType, error) {
	raw := goType{expr: "json.RawMessage", kind: kindRaw}
	if s == nil || s.boolean != nil {
		return raw, nil
	}
	nullable := false
	// A lone allOf member, and "this or null", are the schema they wrap.
	for {
		switch {
		case len(s.allOf) == 1 && s.ref == "" && len(s.types) == 0 && !s.hasProps:
			s = s.allOf[0]
			continue
		case len(s.anyOf) == 2 && len(s.oneOf) == 0 && len(s.allOf) == 0 && s.ref == "" && len(s.types) == 0 && !s.hasProps:
			if inner, ok := orNull(s.anyOf); ok {
				s, nullable = inner, true
				continue
			}
		case len(s.oneOf) == 2 && len(s.anyOf) == 0 && len(s.allOf) == 0 && s.ref == "" && len(s.types) == 0 && !s.hasProps:
			if inner, ok := orNull(s.oneOf); ok {
				s, nullable = inner, true
				continue
			}
		}
		break
	}
	if s == nil || s.boolean != nil {
		return raw, nil
	}
	// A choice between schemas has no single Go type.
	if len(s.anyOf) > 0 || len(s.oneOf) > 0 || len(s.allOf) > 0 {
		return raw, nil
	}
	var t goType
	var err error
	if s.ref != "" {
		t, err = g.refType(rt, s.ref)
	} else {
		base := ""
		for _, name := range s.types {
			switch {
			case name == "null":
				nullable = true
			case base == "":
				base = name
			default:
				return raw, nil // several types: no single Go type
			}
		}
		if base == "" && len(s.types) == 0 {
			base = impliedType(s)
		}
		t, err = g.baseType(rt, s, base, name, path)
	}
	if err != nil {
		return goType{}, err
	}
	// A struct that refers to itself can only hold itself by pointer.
	if (nullable || (t.recursive && direct)) && !t.pointer {
		switch t.kind {
		case kindStruct, kindString, kindEnum, kindInt, kindNumber, kindBool:
			t.expr, t.pointer = "*"+t.expr, true
		}
	}
	return t, nil
}

// orNull is the other member of a two-member choice whose one member is
// the null type.
func orNull(choice []*schema) (*schema, bool) {
	switch {
	case choice[0].isNull() && !choice[1].isNull():
		return choice[1], true
	case choice[1].isNull() && !choice[0].isNull():
		return choice[0], true
	}
	return nil, false
}

// impliedType is the type of a schema that names none but has a keyword
// only one type uses.
func impliedType(s *schema) string {
	switch {
	case s.hasProps || s.additional != nil || s.patternProps:
		return "object"
	case s.items != nil || s.tuple:
		return "array"
	case s.contentEncoding != "":
		return "string"
	case len(s.enum) > 0:
		if _, ok := stringEnum(s.enum); ok {
			return "string"
		}
	}
	return ""
}

// stringEnum is the values of an enum whose values are all strings. A
// null among them is left out: it says the value may be null.
func stringEnum(enum []json.RawMessage) ([]string, bool) {
	var values []string
	for _, raw := range enum {
		if string(raw) == "null" {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return nil, false
		}
		values = append(values, v)
	}
	return values, len(values) > 0
}

func (g *generator) baseType(rt *root, s *schema, base, name, path string) (goType, error) {
	switch base {
	case "string":
		if s.contentEncoding == "base64" || (s.contentEncoding == "" && s.format == "byte") {
			return goType{expr: "[]byte", kind: kindBytes}, nil
		}
		if values, ok := stringEnum(s.enum); ok {
			return g.enumType(rt, s, values, name, path)
		}
		return goType{expr: "string", kind: kindString}, nil
	case "integer":
		return goType{expr: "int64", kind: kindInt}, nil
	case "number":
		return goType{expr: "float64", kind: kindNumber}, nil
	case "boolean":
		return goType{expr: "bool", kind: kindBool}, nil
	case "array":
		if s.items == nil || s.tuple {
			return goType{expr: "[]json.RawMessage", kind: kindSlice, elem: "json.RawMessage"}, nil
		}
		elem, err := g.typeOf(rt, s.items, name+"Item", path+"[]", false)
		if err != nil {
			return goType{}, err
		}
		return goType{expr: "[]" + elem.expr, kind: kindSlice, elem: elem.expr}, nil
	case "object":
		if structShape(s) {
			return g.structType(rt, s, name, path)
		}
		// An object that says nothing of its members, or only what
		// every member is, is a map. One that has properties and admits
		// others, or a property name no struct tag can carry, is a map
		// of anything.
		if !s.hasProps && !s.patternProps && s.additional != nil && s.additional.boolean == nil {
			elem, err := g.typeOf(rt, s.additional, name+"Value", path+"{}", false)
			if err != nil {
				return goType{}, err
			}
			return goType{expr: "map[string]" + elem.expr, kind: kindMap, elem: elem.expr}, nil
		}
		return goType{expr: "map[string]any", kind: kindMap, elem: "any"}, nil
	}
	return goType{expr: "json.RawMessage", kind: kindRaw}, nil
}

// structShape reports an object schema that becomes a struct: it lists
// its properties, or admits none at all, and admits no others.
func structShape(s *schema) bool {
	closed := s.additional != nil && s.additional.boolean != nil && !*s.additional.boolean
	return (s.hasProps || closed) && !s.open() && allTaggable(s.props)
}

func allTaggable(props []property) bool {
	for _, p := range props {
		if !taggable(p.name) {
			return false
		}
	}
	return true
}

// structType declares the struct of an object schema with properties.
func (g *generator) structType(rt *root, s *schema, name, path string) (goType, error) {
	if err := g.declare(name, rt.at(path)); err != nil {
		return goType{}, err
	}
	d := &decl{}
	g.decls = append(g.decls, d)
	var fields []field
	goNames := map[string]string{}
	for _, p := range s.props {
		goName := pascal(p.name)
		if goName == "" {
			goName = "X"
		}
		if other, taken := goNames[goName]; taken {
			return goType{}, fmt.Errorf("clientgen: %s: properties %q and %q would both be the field %s", rt.at(path), other, p.name, goName)
		}
		goNames[goName] = p.name
		t, err := g.typeOf(rt, p.schema, name+goName, path+"."+p.name, true)
		if err != nil {
			return goType{}, err
		}
		optional := !s.required[p.name]
		// An optional object is a pointer: omitempty never leaves a
		// struct out, and an absent object is not an object of zeros.
		if optional && t.kind == kindStruct && !t.pointer {
			t.expr, t.pointer = "*"+t.expr, true
		}
		fields = append(fields, field{goName: goName, jsonName: p.name, typ: t, doc: p.schema.description, optional: optional})
		d.json = d.json || strings.Contains(t.expr, "json.RawMessage")
	}
	var b strings.Builder
	typeDoc(&b, rt, s, name, path, "")
	fmt.Fprintf(&b, "type %s struct {\n", name)
	for _, f := range fields {
		if f.doc != "" {
			comment(&b, "\t", f.doc)
		}
		fmt.Fprintf(&b, "\t%s %s %s\n", f.goName, f.typ.expr, jsonTag(f.jsonName, f.optional))
	}
	b.WriteString("}\n\n")
	d.text = b.String()
	g.structs[name] = fields
	return goType{expr: name, kind: kindStruct}, nil
}

// typeDoc writes the comment of a generated type: what it is the type of,
// then the schema's own description.
func typeDoc(b *strings.Builder, rt *root, s *schema, name, path, more string) {
	what := path + " of " + rt.ref
	if path == "input" || path == "output" {
		what = "the " + what
	}
	comment(b, "", name+" is "+what+more+".")
	if s != nil && s.description != "" {
		comment(b, "", "\n"+s.description)
	}
}

// enumType declares a string type and a constant for each value.
func (g *generator) enumType(rt *root, s *schema, values []string, name, path string) (goType, error) {
	if err := g.declare(name, rt.at(path)); err != nil {
		return goType{}, err
	}
	var b strings.Builder
	typeDoc(&b, rt, s, name, path, ": one of the values below")
	fmt.Fprintf(&b, "type %s string\n\n", name)
	fmt.Fprintf(&b, "// The values of %s.\nconst (\n", name)
	for _, v := range values {
		part := pascal(v)
		if v == "" {
			part = "Empty"
		}
		if part == "" {
			return goType{}, fmt.Errorf("clientgen: %s: enum value %q has no letter or digit to make a Go name from", rt.at(path), v)
		}
		if err := g.declare(name+part, fmt.Sprintf("%s value %q", rt.at(path), v)); err != nil {
			return goType{}, err
		}
		fmt.Fprintf(&b, "\t%s %s = %q\n", name+part, name, v)
	}
	b.WriteString(")\n\n")
	g.decls = append(g.decls, &decl{text: b.String()})
	return goType{expr: name, kind: kindEnum}, nil
}

// refType resolves a $ref inside its own schema: "#" and "#/$defs/Name"
// (or "#/definitions/Name"). A definition that is an object with
// properties, or a string enum, becomes one named type, however often it
// is referred to. Any other definition is expanded where it is used.
// A reference that leaves the schema, or a definition that refers to
// itself without being a struct, has no Go type that can be named: it is
// json.RawMessage.
func (g *generator) refType(rt *root, ref string) (goType, error) {
	raw := goType{expr: "json.RawMessage", kind: kindRaw}
	if ref == "#" {
		// The root is being generated whenever one of its own schemas
		// is: the reference is recursive.
		if !namedShape(rt.schema) {
			return raw, nil
		}
		return goType{expr: rt.name, kind: kindStruct, recursive: true}, nil
	}
	if !strings.HasPrefix(ref, "#/$defs/") && !strings.HasPrefix(ref, "#/definitions/") {
		return raw, nil
	}
	key := ref[strings.LastIndex(ref, "/")+1:]
	key = strings.NewReplacer("~1", "/", "~0", "~").Replace(key)
	target := rt.schema.defs[key]
	if target == nil {
		return raw, nil
	}
	st := rt.defs[key]
	switch {
	case st == nil:
		st = &defState{name: rt.name + pascal(key)}
		rt.defs[key] = st
		t, err := g.typeOf(rt, target, st.name, rt.path+ref, false)
		if err != nil {
			return goType{}, err
		}
		st.typ, st.done = t, true
		return t, nil
	case st.done:
		return st.typ, nil
	case namedShape(target):
		return goType{expr: st.name, kind: kindStruct, recursive: true}, nil
	}
	return raw, nil // refers to itself and is not a struct
}

// namedShape reports a schema that becomes a named struct: only such a
// type can refer to itself.
func namedShape(s *schema) bool {
	if s == nil || s.boolean != nil || s.ref != "" || len(s.anyOf)+len(s.oneOf)+len(s.allOf) > 0 {
		return false
	}
	object := len(s.types) == 0 || (len(s.types) == 1 && s.types[0] == "object")
	return object && structShape(s)
}
