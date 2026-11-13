package clientgen

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/craggy-io/capability-services/registry"
)

var spaces = regexp.MustCompile(`[ \t]+`)

// flat is generated source with every run of spaces and tabs made one
// space, so a test can name a declaration without knowing how gofmt
// aligned it.
func flat(src []byte) string { return spaces.ReplaceAllString(string(src), " ") }

// desc is a descriptor with the given schemas; an empty one is no schema.
func desc(name string, version int, in, out string) registry.Descriptor {
	d := registry.Descriptor{Name: name, Version: version}
	if in != "" {
		d.Input = json.RawMessage(in)
	}
	if out != "" {
		d.Output = json.RawMessage(out)
	}
	return d
}

// source generates one file for the descriptors and returns it flattened.
func source(t *testing.T, descs ...registry.Descriptor) string {
	t.Helper()
	files, err := Generate(descs, Options{Package: "client"})
	if err != nil {
		t.Fatal(err)
	}
	return flat(files["client.go"])
}

// wantAll fails for each wanted text the source lacks.
func wantAll(t *testing.T, src string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(src, want) {
			t.Errorf("generated source lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(src)
	}
}

// outputOf generates capability t.x with the given output schema.
func outputOf(t *testing.T, out string) string {
	t.Helper()
	return source(t, desc("t.x", 1, "", out))
}

func TestObjectsBecomeStructs(t *testing.T) {
	src := outputOf(t, `{"type":"object","properties":{
		"id":{"type":"string","description":"the id"},
		"count":{"type":"integer"},
		"ratio":{"type":"number"},
		"ok":{"type":"boolean"},
		"note":{"type":"string"},
		"size":{"type":"integer"},
		"share":{"type":"number"},
		"done":{"type":"boolean"}},
		"required":["id","count","ratio","ok"]}`)
	wantAll(t, src,
		"type TXOut struct {",
		"// the id\n Id string `json:\"id\"`",
		"Count int64 `json:\"count\"`",
		"Ratio float64 `json:\"ratio\"`",
		"Ok bool `json:\"ok\"`",
		// Optional scalars are values with omitempty.
		"Note string `json:\"note,omitempty\"`",
		"Size int64 `json:\"size,omitempty\"`",
		"Share float64 `json:\"share,omitempty\"`",
		"Done bool `json:\"done,omitempty\"`",
	)
	// Fields keep the order of the schema's properties.
	if strings.Index(src, "Ratio float64") > strings.Index(src, "Note string") {
		t.Error("fields are not in property order")
	}
}

func TestArraysMapsAndBytes(t *testing.T) {
	src := outputOf(t, `{"type":"object","properties":{
		"names":{"type":"array","items":{"type":"string"}},
		"grid":{"type":"array","items":{"type":"array","items":{"type":"integer"}}},
		"rows":{"type":["null","array"],"items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}},
		"loose":{"type":"array"},
		"tuple":{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}]},
		"labels":{"type":"object","additionalProperties":{"type":"string"}},
		"files":{"type":"object","additionalProperties":{"type":"string","contentEncoding":"base64"}},
		"parts":{"type":"object","additionalProperties":{"type":"object","properties":{"n":{"type":"integer"}}}},
		"any":{"type":"object"},
		"anyToo":{"type":"object","additionalProperties":true},
		"open":{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":true},
		"none":{"type":"object","additionalProperties":false},
		"data":{"type":"string","contentEncoding":"base64"},
		"blob":{"type":"string","format":"byte"},
		"when":{"type":"string","format":"date-time"}},
		"required":["names","rows","data"]}`)
	wantAll(t, src,
		"Names []string `json:\"names\"`",
		"Grid [][]int64 `json:\"grid,omitempty\"`",
		"Rows []TXOutRowsItem `json:\"rows\"`", // a nullable array is a slice: nil is null
		"type TXOutRowsItem struct {",
		"Loose []json.RawMessage `json:\"loose,omitempty\"`",
		"Tuple []json.RawMessage `json:\"tuple,omitempty\"`",
		"Labels map[string]string `json:\"labels,omitempty\"`",
		"Files map[string][]byte `json:\"files,omitempty\"`",
		"Parts map[string]TXOutPartsValue `json:\"parts,omitempty\"`",
		"type TXOutPartsValue struct {",
		"Any map[string]any `json:\"any,omitempty\"`",
		"AnyToo map[string]any `json:\"anyToo,omitempty\"`",
		"Open map[string]any `json:\"open,omitempty\"`", // properties and more: no struct holds the more
		"None *TXOutNone `json:\"none,omitempty\"`",
		"type TXOutNone struct {\n}",
		"Data []byte `json:\"data\"`",
		"Blob []byte `json:\"blob,omitempty\"`",
		"When string `json:\"when,omitempty\"`", // a time stays the text that was sent
	)
}

func TestNestedObjectsAndNull(t *testing.T) {
	src := outputOf(t, `{"type":"object","properties":{
		"must":{"type":"object","properties":{"a":{"type":"string"}}},
		"may":{"type":"object","properties":{"a":{"type":"string"}}},
		"mustOrNull":{"type":["null","object"],"properties":{"a":{"type":"string"}}},
		"text":{"type":["string","null"]},
		"count":{"type":["null","integer"]},
		"maybe":{"anyOf":[{"type":"boolean"},{"type":"null"}]},
		"either":{"oneOf":[{"type":"null"},{"type":"number"}]},
		"list":{"type":["array","null"],"items":{"type":"string"}},
		"bytes":{"type":["string","null"],"contentEncoding":"base64"},
		"wrapped":{"allOf":[{"type":"string"}],"description":"one member"}},
		"required":["must","mustOrNull","text","maybe"]}`)
	wantAll(t, src,
		"Must TXOutMust `json:\"must\"`",                              // required object: a value
		"May *TXOutMay `json:\"may,omitempty\"`",                      // optional object: a pointer
		"MustOrNull *TXOutMustOrNull `json:\"mustOrNull\"`",           // nullable object: a pointer, sent as null
		"Text *string `json:\"text\"`",                                // nullable scalar: a pointer
		"Count *int64 `json:\"count,omitempty\"`",                     // nullable and optional
		"Maybe *bool `json:\"maybe\"`",                                // anyOf [T, null] is a nullable T
		"Either *float64 `json:\"either,omitempty\"`",                 // so is oneOf
		"List []string `json:\"list,omitempty\"`",                     // a slice is already nullable
		"Bytes []byte `json:\"bytes,omitempty\"`",                     // so is []byte
		"// one member\n Wrapped string `json:\"wrapped,omitempty\"`", // a lone allOf member is that member
	)
}

func TestEnums(t *testing.T) {
	src := outputOf(t, `{"type":"object","properties":{
		"state":{"type":"string","enum":["queued","in-progress","done",""],"description":"where it is"},
		"levels":{"type":"array","items":{"enum":["low","high"]}},
		"orNull":{"type":["string","null"],"enum":["a","b",null]},
		"code":{"type":"integer","enum":[1,2,3]}},
		"required":["state"]}`)
	wantAll(t, src,
		"State TXOutState `json:\"state\"`",
		"// where it is\ntype TXOutState string",
		"TXOutStateQueued TXOutState = \"queued\"",
		"TXOutStateInProgress TXOutState = \"in-progress\"",
		"TXOutStateDone TXOutState = \"done\"",
		"TXOutStateEmpty TXOutState = \"\"",
		"Levels []TXOutLevelsItem `json:\"levels,omitempty\"`", // an enum of strings needs no "type"
		"TXOutLevelsItemLow TXOutLevelsItem = \"low\"",
		"OrNull *TXOutOrNull `json:\"orNull,omitempty\"`",
		"TXOutOrNullA TXOutOrNull = \"a\"",
		"Code int64 `json:\"code,omitempty\"`", // only strings get constants
	)
}

func TestNoSingleGoType(t *testing.T) {
	src := outputOf(t, `{"type":"object","properties":{
		"anything":{},
		"alsoAnything":true,
		"described":{"description":"any JSON"},
		"choice":{"oneOf":[{"type":"string"},{"type":"integer"}]},
		"choice2":{"anyOf":[{"type":"string"},{"type":"integer"},{"type":"null"}]},
		"both":{"allOf":[{"type":"object"},{"type":"object"}]},
		"mixed":{"type":["string","integer"]},
		"nothing":{"type":"null"},
		"elsewhere":{"$ref":"https://example.com/schema.json"},
		"missing":{"$ref":"#/$defs/Missing"},
		"deep":{"$ref":"#/properties/anything"}},
		"required":["anything"]}`)
	for _, name := range []string{"Anything", "AlsoAnything", "Described", "Choice", "Choice2", "Both", "Mixed", "Nothing", "Elsewhere", "Missing", "Deep"} {
		if !strings.Contains(src, name+" json.RawMessage `json:") {
			t.Errorf("%s is not json.RawMessage", name)
		}
	}
	if t.Failed() {
		t.Log(src)
	}
}

func TestDefinitions(t *testing.T) {
	src := outputOf(t, `{"type":"object",
		"properties":{
			"first":{"$ref":"#/$defs/Point"},
			"second":{"$ref":"#/$defs/Point","description":"the far corner"},
			"points":{"type":"array","items":{"$ref":"#/$defs/Point"}},
			"unit":{"$ref":"#/definitions/Unit"},
			"ids":{"$ref":"#/$defs/Ids"},
			"tree":{"$ref":"#/$defs/Node"},
			"self":{"$ref":"#"},
			"loop":{"$ref":"#/$defs/Loop"}},
		"required":["first","tree"],
		"$defs":{
			"Point":{"type":"object","description":"A place.","properties":{"x":{"type":"number"},"y":{"type":"number"}},"required":["x","y"]},
			"Ids":{"type":"array","items":{"type":"string"}},
			"Node":{"type":"object","properties":{
				"next":{"$ref":"#/$defs/Node"},
				"prev":{"$ref":"#/$defs/Node"},
				"kids":{"type":"array","items":{"$ref":"#/$defs/Node"}},
				"byName":{"type":"object","additionalProperties":{"$ref":"#/$defs/Node"}}},
				"required":["next"]},
			"Loop":{"type":"array","items":{"$ref":"#/$defs/Loop"}}},
		"definitions":{"Unit":{"enum":["m","km"]}}}`)
	wantAll(t, src,
		"First TXOutPoint `json:\"first\"`", // one named type, however often it is referred to
		"// the far corner\n Second *TXOutPoint `json:\"second,omitempty\"`",
		"Points []TXOutPoint `json:\"points,omitempty\"`",
		"// A place.\ntype TXOutPoint struct {",
		"Unit TXOutUnit `json:\"unit,omitempty\"`",
		"TXOutUnitKm TXOutUnit = \"km\"",
		"Ids []string `json:\"ids,omitempty\"`", // a definition that is no struct or enum is expanded in place
		"Tree TXOutNode `json:\"tree\"`",
		"Next *TXOutNode `json:\"next\"`", // a struct holds itself by pointer, required or not
		"Prev *TXOutNode `json:\"prev,omitempty\"`",
		"Kids []TXOutNode `json:\"kids,omitempty\"`",
		"ByName map[string]TXOutNode `json:\"byName,omitempty\"`",
		"Self *TXOut `json:\"self,omitempty\"`",
		"Loop []json.RawMessage `json:\"loop,omitempty\"`", // recursion with no struct to name
	)
	if n := strings.Count(src, "type TXOutPoint struct"); n != 1 {
		t.Errorf("TXOutPoint declared %d times", n)
	}
}

func TestPropertyNames(t *testing.T) {
	src := outputOf(t, `{"type":"object","properties":{
		"content-type":{"type":"string"},
		"$schema":{"type":"string"},
		"2fa":{"type":"boolean"},
		"snake_case":{"type":"string"},
		"artifactId":{"type":"string"},
		"with space":{"type":"string"},
		"-":{"type":"string"},
		"type":{"type":"string"},
		"func":{"type":"string"}},
		"required":["-"]}`)
	wantAll(t, src,
		"ContentType string `json:\"content-type,omitempty\"`",
		"Schema string `json:\"$schema,omitempty\"`",
		"X2fa bool `json:\"2fa,omitempty\"`",
		"SnakeCase string `json:\"snake_case,omitempty\"`",
		"ArtifactId string `json:\"artifactId,omitempty\"`",
		"WithSpace string `json:\"with space,omitempty\"`",
		"X string `json:\"-,\"`", // "-" alone would skip the field
		"Type string `json:\"type,omitempty\"`",
		"Func string `json:\"func,omitempty\"`",
	)
	// A name no struct tag can carry makes the object a map.
	src = outputOf(t, `{"type":"object","properties":{"in":{"type":"object","properties":{"a,b":{"type":"string"},"c":{"type":"string"}}}}}`)
	wantAll(t, src, "In map[string]any `json:\"in,omitempty\"`")
}

func TestRootTypes(t *testing.T) {
	cases := []struct{ in, out, want string }{
		{"", "", "type TXIn struct {\n}"},                                  // no input schema: the empty object
		{"", "", "type TXOut = json.RawMessage"},                           // no output schema: any JSON
		{`{"type":"object","properties":{}}`, "", "type TXIn struct {\n}"}, // the registry's no-input schema
		{`{"type":"object"}`, "", "type TXIn = map[string]any"},
		{"", `{"type":"array","items":{"type":"string"}}`, "type TXOut = []string"},
		{"", `{"type":"string"}`, "type TXOut = string"},
		{"", `{"type":"object","additionalProperties":{"type":"integer"}}`, "type TXOut = map[string]int64"},
		{"", `{"type":["null","object"],"properties":{"a":{"type":"string"}}}`, "type TXOut struct {"},
		{"", `{"oneOf":[{"type":"string"},{"type":"integer"}]}`, "type TXOut = json.RawMessage"},
		{"", `{"type":"array","items":{"type":"object","properties":{"a":{"type":"string"}}}}`, "type TXOut = []TXOutItem"},
		{"", `{"$ref":"#/$defs/P","$defs":{"P":{"type":"object","properties":{"a":{"type":"string"}}}}}`, "type TXOut = TXOutP"},
	}
	for _, c := range cases {
		src := source(t, desc("t.x", 1, c.in, c.out))
		if !strings.Contains(src, c.want) {
			t.Errorf("in %s out %s: source lacks %q\n%s", c.in, c.out, c.want, src)
		}
	}
	// A map input is never sent as null.
	src := source(t, desc("t.x", 1, `{"type":"object"}`, ""))
	wantAll(t, src, "if in == nil {\n in = TXIn{}\n }")
}

func TestNamesAndComments(t *testing.T) {
	d := desc("artifacts.runs.record-tree", 1, "", "")
	d.Title = "Record a run."
	d.Description = "Records a run whose output is a folder.\n\nIdentical runs are reused."
	d.Long = true
	src := source(t, d, desc("a.b_c.d2", 3, "", ""))
	wantAll(t, src,
		"// Code generated by capability-services clientgen. DO NOT EDIT.\n\npackage client\n",
		"const ArtifactsRunsRecordTreeRef = \"artifacts.runs.record-tree@1\"",
		"// ArtifactsRunsRecordTree calls artifacts.runs.record-tree@1: Record a run.\n//\n// Records a run whose output is a folder.\n//\n// Identical runs are reused.\n//\n// The capability is long-running.",
		"func ArtifactsRunsRecordTree(ctx context.Context, c Caller, in ArtifactsRunsRecordTreeIn) (ArtifactsRunsRecordTreeOut, error) {",
		"// ArtifactsRunsRecordTreeIn is the input of artifacts.runs.record-tree@1.",
		"const ABCD2Ref = \"a.b_c.d2@3\"", // one version generated: no version in the name
		"func ABCD2(ctx context.Context",
		"type Caller interface {\n Call(ctx context.Context, ref string, in any) (json.RawMessage, error)\n}",
	)
	if strings.Contains(src, "craggy-io") {
		t.Error("generated code names this module")
	}
}

// A file imports what its code uses, whatever its comments mention.
func TestImportsFollowTheCode(t *testing.T) {
	d := desc("a.b", 1, `{"type":"object","properties":{"x":{"type":"string","description":"not a json.RawMessage, an iter.Seq2 or a fmt.Errorf"}}}`,
		`{"type":"object","properties":{"y":{"type":"string"}}}`)
	d.Description = "Mentions json.RawMessage."
	files, err := Generate([]registry.Descriptor{d, desc("c.d", 1, "", "")}, Options{Package: "client", Group: ByNamespace})
	if err != nil {
		t.Fatal(err)
	}
	if src := string(files["a_client.go"]); !strings.Contains(src, "import (\n\t\"context\"\n)") {
		t.Errorf("a_client.go imports more than context:\n%s", src)
	}
	if src := string(files["c_client.go"]); !strings.Contains(src, "import (\n\t\"context\"\n\t\"encoding/json\"\n)") {
		t.Errorf("c_client.go does not import context and encoding/json:\n%s", src)
	}
}

func TestVersions(t *testing.T) {
	descs := []registry.Descriptor{desc("t.ver", 1, "", ""), desc("t.ver", 2, "", ""), desc("t.one", 5, "", "")}
	// Every version of a name generated together carries its version.
	src := source(t, descs...)
	wantAll(t, src, "const TVerV1Ref = \"t.ver@1\"", "const TVerV2Ref = \"t.ver@2\"", "func TVerV2(", "type TVerV1In struct", "const TOneRef = \"t.one@5\"")
	// One version generated: the bare name, whichever version it is.
	files, err := Generate(descs, Options{Package: "client", Include: []string{"t.ver@1"}})
	if err != nil {
		t.Fatal(err)
	}
	wantAll(t, flat(files["client.go"]), "const TVerRef = \"t.ver@1\"")
	// A bare name is the highest version described.
	files, err = Generate(descs, Options{Package: "client", Include: []string{"t.ver"}})
	if err != nil {
		t.Fatal(err)
	}
	src = flat(files["client.go"])
	wantAll(t, src, "const TVerRef = \"t.ver@2\"")
	if strings.Contains(src, "TOne") {
		t.Error("a capability that was not included was generated")
	}
}

func TestErrors(t *testing.T) {
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	cases := []struct {
		name  string
		descs []registry.Descriptor
		opts  Options
		want  string
	}{
		{"two capabilities, one name", []registry.Descriptor{desc("a.b-c", 1, "", ""), desc("a.b.c", 1, "", "")}, Options{Package: "p"},
			"a.b-c@1 and a.b.c@1 would both be named ABC"},
		{"a function and a type", []registry.Descriptor{desc("a.b", 1, "", ""), desc("a.b.in", 1, "", "")}, Options{Package: "p"},
			"would both be named ABIn"},
		{"a paging function and a capability", []registry.Descriptor{
			desc("a.list", 1, obj(`"cursor":{"type":"string"}`), obj(`"items":{"type":"array"},"next":{"type":"string"}`)),
			desc("a.list.all", 1, "", "")}, Options{Package: "p"},
			"would both be named AListAll"},
		{"two properties, one field", []registry.Descriptor{desc("a.b", 1, obj(`"foo_bar":{"type":"string"},"foo-bar":{"type":"string"}`), "")}, Options{Package: "p"},
			`a.b@1 input: properties "foo_bar" and "foo-bar" would both be the field FooBar`},
		{"a nested type and a capability", []registry.Descriptor{
			desc("a.b", 1, obj(`"x":{"type":"object","properties":{}}`), ""), desc("a.b-in-x", 1, "", "")}, Options{Package: "p"},
			"would both be named ABInX"},
		{"two enum values, one constant", []registry.Descriptor{desc("a.b", 1, obj(`"e":{"enum":["a-b","a_b"]}`), "")}, Options{Package: "p"},
			`would both be named ABInEAB`},
		{"an enum value with no name", []registry.Descriptor{desc("a.b", 1, obj(`"e":{"enum":["+","-"]}`), "")}, Options{Package: "p"},
			`enum value "+" has no letter or digit`},
		{"the package's own name", []registry.Descriptor{desc("caller", 1, "", "")}, Options{Package: "p"},
			"would be named Caller, which the generated package declares itself"},
		{"described twice", []registry.Descriptor{desc("a.b", 1, "", ""), desc("a.b", 0, "", "")}, Options{Package: "p"},
			"a.b@1 is described twice"},
		{"not described", []registry.Descriptor{desc("a.b", 1, "", "")}, Options{Package: "p", Include: []string{"a.b@2"}},
			"no descriptor for a.b@2"},
		{"bare name not described", []registry.Descriptor{desc("a.b", 1, "", "")}, Options{Package: "p", Include: []string{"a.c"}},
			"no descriptor for a.c"},
		{"no package", []registry.Descriptor{desc("a.b", 1, "", "")}, Options{},
			`package name "" is not a Go identifier`},
		{"bad package", []registry.Descriptor{desc("a.b", 1, "", "")}, Options{Package: "my-client"},
			`package name "my-client" is not a Go identifier`},
		{"schema is not JSON", []registry.Descriptor{desc("a.b", 1, `{"type":`, "")}, Options{Package: "p"},
			"a.b@1 input schema"},
		{"schema is not a schema", []registry.Descriptor{desc("a.b", 1, "", `[1]`)}, Options{Package: "p"},
			"a.b@1 output schema"},
		{"no name", []registry.Descriptor{desc("...", 1, "", "")}, Options{Package: "p"},
			"has no letter or digit to make a Go name from"},
	}
	for _, c := range cases {
		_, err := Generate(c.descs, c.opts)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want one containing %q", c.name, err, c.want)
		}
	}
}

func TestPagingDetection(t *testing.T) {
	obj := func(props string) string { return `{"type":"object","properties":{` + props + `}}` }
	cursorIn := obj(`"limit":{"type":"integer"},"cursor":{"type":"string"}`)
	offsetIn := obj(`"id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}`)
	cases := []struct {
		name, in, out string
		all           string // the result type of All; empty when the capability is not paged
	}{
		{"the library's Page", cursorIn, obj(`"items":{"type":"array","items":{"type":"string"}},"next":{"type":"string"}`), "[]string"},
		{"a list under another name", cursorIn, obj(`"orders":{"type":["null","array"],"items":{"type":"integer"}},"next":{"type":"string"},"total":{"type":"integer"}`), "[]int64"},
		{"two lists, one named items", cursorIn, obj(`"refs":{"type":"array","items":{"type":"string"}},"items":{"type":"array","items":{"type":"integer"}},"next":{"type":"string"}`), "[]int64"},
		{"two lists, none named items", cursorIn, obj(`"a":{"type":"array"},"b":{"type":"array"},"next":{"type":"string"}`), ""},
		{"no list", cursorIn, obj(`"count":{"type":"integer"},"next":{"type":"string"}`), ""},
		{"no next", cursorIn, obj(`"items":{"type":"array"}`), ""},
		{"no cursor", obj(`"limit":{"type":"integer"}`), obj(`"items":{"type":"array"},"next":{"type":"string"}`), ""},
		{"a cursor that is not a string", obj(`"cursor":{"type":"integer"}`), obj(`"items":{"type":"array"},"next":{"type":"string"}`), ""},
		{"a nullable next", cursorIn, obj(`"items":{"type":"array"},"next":{"type":["string","null"]}`), ""},
		{"no output schema", cursorIn, "", ""},
		{"bytes by offset", offsetIn, obj(`"data":{"type":"string","contentEncoding":"base64"},"eof":{"type":"boolean"}`), "[]byte"},
		{"rows by offset", offsetIn, obj(`"rows":{"type":"array","items":{"type":"object"}},"eof":{"type":"boolean"},"total":{"type":"integer"}`), "[]map[string]any"},
		{"offset without eof", offsetIn, obj(`"data":{"type":"string","contentEncoding":"base64"}`), ""},
		{"eof without offset", obj(`"id":{"type":"string"}`), obj(`"data":{"type":"string","contentEncoding":"base64"},"eof":{"type":"boolean"}`), ""},
		{"an offset that is not an integer", obj(`"offset":{"type":"string"}`), obj(`"data":{"type":"string","contentEncoding":"base64"},"eof":{"type":"boolean"}`), ""},
	}
	for _, c := range cases {
		src := source(t, desc("t.x", 1, c.in, c.out))
		paged := strings.Contains(src, "func TXPages(ctx context.Context, c Caller, in TXIn) iter.Seq2[TXOut, error] {")
		if paged != (c.all != "") {
			t.Errorf("%s: paged = %v", c.name, paged)
			continue
		}
		if paged && !strings.Contains(src, "func TXAll(ctx context.Context, c Caller, in TXIn) ("+c.all+", error) {") {
			t.Errorf("%s: TXAll does not return %s\n%s", c.name, c.all, src)
		}
	}
}

func TestGrouping(t *testing.T) {
	descs := []registry.Descriptor{desc("b.x", 1, "", ""), desc("a.y", 1, "", ""), desc("a-linux.z", 1, "", ""), desc("a.x", 1, "", "")}
	files, err := Generate(descs, Options{Package: "client", Group: ByNamespace})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name, src := range files {
		names = append(names, name)
		if !strings.HasPrefix(string(src), Header+"\n\npackage client\n") {
			t.Errorf("%s does not start with the header and the package", name)
		}
	}
	// A namespace never makes a file name the go tool reads as a build
	// constraint.
	for _, want := range []string{"client.go", "a_client.go", "b_client.go", "a_linux_client.go"} {
		if _, ok := files[want]; !ok {
			t.Errorf("no file %s among %v", want, names)
		}
	}
	if len(files) != 4 {
		t.Errorf("files = %v", names)
	}
	if a := flat(files["a_client.go"]); !strings.Contains(a, "func AX(") || !strings.Contains(a, "func AY(") || strings.Contains(a, "func BX(") {
		t.Errorf("a_client.go holds the wrong capabilities:\n%s", a)
	}
	if strings.Contains(string(files["client.go"]), "func AX(") {
		t.Error("client.go holds a capability")
	}
	// Grouping changes the files, never a name.
	one, err := Generate(descs, Options{Package: "client"})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || !strings.Contains(string(one["client.go"]), "func AX(") {
		t.Errorf("one file: %d files", len(one))
	}
}

func TestDeterministic(t *testing.T) {
	a := []registry.Descriptor{desc("b.x", 1, "", ""), desc("a.y", 2, "", ""), desc("a.y", 1, "", "")}
	b := []registry.Descriptor{a[2], a[0], a[1]}
	first, err := Generate(a, Options{Package: "client"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := Generate(b, Options{Package: "client"})
		if err != nil {
			t.Fatal(err)
		}
		if string(again["client.go"]) != string(first["client.go"]) {
			t.Fatal("the same descriptors in another order gave other bytes")
		}
	}
	src := string(first["client.go"])
	if !(strings.Index(src, "AYV1Ref") < strings.Index(src, "AYV2Ref") && strings.Index(src, "AYV2Ref") < strings.Index(src, "BXRef")) {
		t.Error("capabilities are not in name, then version, order")
	}
}

func TestParseDescriptors(t *testing.T) {
	list := `[{"name":"a.b","version":2,"title":"T","input":{"type":"object"},"long":true}]`
	for name, data := range map[string]string{
		"a list":           list,
		"an HTTP listing":  `{"capabilities":` + list + `}`,
		"a describe reply": `{"type":"describe-reply","id":"d1","capabilities":` + list + `,"groups":[{"name":"a","description":"A"}]}`,
	} {
		descs, err := ParseDescriptors([]byte(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(descs) != 1 || descs[0].Name != "a.b" || descs[0].Version != 2 || descs[0].Title != "T" || !descs[0].Long || string(descs[0].Input) != `{"type":"object"}` {
			t.Errorf("%s: %+v", name, descs)
		}
	}
	for name, data := range map[string]string{
		"not JSON":        `{`,
		"no capabilities": `{"type":"hello"}`,
		"no name":         `[{"version":1}]`,
	} {
		if _, err := ParseDescriptors([]byte(data)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A provider that describes nothing describes an empty list.
	if descs, err := ParseDescriptors([]byte(`{"capabilities":[]}`)); err != nil || len(descs) != 0 {
		t.Errorf("empty list: %v %v", descs, err)
	}
}
