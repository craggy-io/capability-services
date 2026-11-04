package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// Bounds on the invalid-input message, so a badly wrong call cannot turn
// its error into a wall of text.
const (
	maxInvalidMessage = 1500 // bytes, whole message
	maxProblems       = 8    // problem lines listed before "... and N more"
	maxProblemLine    = 240  // bytes per problem line
	maxExampleText    = 600  // bytes of example JSON
	maxValueText      = 60   // bytes of a quoted offending value
	maxNamesListed    = 12   // field names / enum values per list
	maxExplainDepth   = 32   // schema nesting followed
)

// invalidInputMessage explains why raw failed c's input schema to a caller
// that has to fix the call (often an LLM agent): one line per offending
// field path saying what is wrong in plain words, the capability's
// required top-level fields and its first example. It is the message of
// the Invalid error Invoke and Start return, so every projection (HTTP,
// MCP, tools, Starlark) carries the same text. cause is the validator's
// own error, used only when the schema walk finds nothing to say. The
// result is at most maxInvalidMessage bytes.
func invalidInputMessage(c Capability, raw json.RawMessage, cause error) string {
	x := &explainer{root: c.Input}
	var inst any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&inst) == nil {
		x.walk("", c.Input, normalizeNumber(inst), 0)
	}
	if len(x.problems) == 0 && cause != nil {
		x.problems = append(x.problems, problem{"", validatorText(cause)})
	}

	var tail strings.Builder
	if req := x.deref(c.Input).Required; len(req) > 0 {
		tail.WriteString("\nRequired fields: " + nameList(req) + ".")
	}
	if len(c.Examples) > 0 {
		tail.WriteString("\nExample input: " + clip(string(c.Examples[0].Input), maxExampleText))
	}

	var b strings.Builder
	b.WriteString(c.Ref() + ": invalid input:")
	const moreReserve = 32 // room for the "... and N more" line
	budget := maxInvalidMessage - tail.Len() - moreReserve
	listed := 0
	for _, p := range x.problems {
		line := "\n- " + clip(p.String(), maxProblemLine)
		if listed == maxProblems || b.Len()+len(line) > budget {
			break
		}
		b.WriteString(line)
		listed++
	}
	if rest := len(x.problems) - listed; rest > 0 {
		fmt.Fprintf(&b, "\n- ... and %d more", rest)
	}
	b.WriteString(tail.String())
	return clip(b.String(), maxInvalidMessage)
}

type problem struct{ path, what string }

func (p problem) String() string {
	if p.path == "" {
		return "input: " + p.what
	}
	return p.path + ": " + p.what
}

// explainer walks an instance against a schema and records what is wrong
// in words. It explains a failure the validator already found; it is not
// a validator and covers only the keywords a caller can act on.
type explainer struct {
	root     *jsonschema.Schema
	problems []problem
}

func (x *explainer) add(path, format string, args ...any) {
	x.problems = append(x.problems, problem{path, fmt.Sprintf(format, args...)})
}

// deref follows local $refs ("#/$defs/T", "#/definitions/T") from the root
// schema. Anything else is returned as is.
func (x *explainer) deref(s *jsonschema.Schema) *jsonschema.Schema {
	for i := 0; s != nil && s.Ref != "" && i < maxExplainDepth; i++ {
		var next *jsonschema.Schema
		if name, ok := strings.CutPrefix(s.Ref, "#/$defs/"); ok && x.root != nil {
			next = x.root.Defs[name]
		} else if name, ok := strings.CutPrefix(s.Ref, "#/definitions/"); ok && x.root != nil {
			next = x.root.Definitions[name]
		} else if s.Ref == "#" {
			next = x.root
		}
		if next == nil {
			return s
		}
		s = next
	}
	if s == nil {
		return &jsonschema.Schema{}
	}
	return s
}

func (x *explainer) walk(path string, s *jsonschema.Schema, v any, depth int) {
	s = x.deref(s)
	if s == nil || depth > maxExplainDepth {
		return
	}
	if isFalseSchema(s) {
		x.add(path, "not allowed here")
		return
	}
	if types := schemaTypes(s); len(types) > 0 && !typeMatches(v, types) {
		x.add(path, "wrong type: expected %s, got %s", strings.Join(types, " or "), describeValue(v))
		return
	}
	if len(s.Enum) > 0 && !containsValue(s.Enum, v) {
		x.add(path, "%s is not an allowed value; use one of: %s", jsonText(v), valueList(s.Enum))
		return
	}
	if s.Const != nil && !sameValue(*s.Const, v) {
		x.add(path, "must be %s, got %s", jsonText(*s.Const), jsonText(v))
		return
	}
	for _, sub := range s.AllOf {
		x.walk(path, sub, v, depth+1)
	}
	if alts := append(append([]*jsonschema.Schema(nil), s.AnyOf...), s.OneOf...); len(alts) > 0 {
		x.alternatives(path, alts, v, depth)
	}
	switch t := v.(type) {
	case int64, float64:
		x.number(path, s, toFloat(t), jsonText(v))
	case string:
		x.str(path, s, t)
	case []any:
		x.array(path, s, t, depth)
	case map[string]any:
		x.object(path, s, t, depth)
	}
}

// alternatives explains an anyOf/oneOf: nothing when some branch fits,
// the one branch whose type fits when there is exactly one (a nullable
// field, say), else a single line.
func (x *explainer) alternatives(path string, alts []*jsonschema.Schema, v any, depth int) {
	var typed []*jsonschema.Schema
	for _, alt := range alts {
		sub := &explainer{root: x.root}
		sub.walk(path, alt, v, depth+1)
		if len(sub.problems) == 0 {
			return
		}
		if t := schemaTypes(x.deref(alt)); len(t) == 0 || typeMatches(v, t) {
			typed = append(typed, alt)
		}
	}
	if len(typed) == 1 {
		x.walk(path, typed[0], v, depth+1)
		return
	}
	var kinds []string
	for _, alt := range alts {
		kinds = append(kinds, schemaTypes(x.deref(alt))...)
	}
	if len(kinds) > 0 {
		x.add(path, "matches none of the allowed forms (%s), got %s", strings.Join(dedupe(kinds), ", "), describeValue(v))
	} else {
		x.add(path, "matches none of the allowed forms")
	}
}

func (x *explainer) number(path string, s *jsonschema.Schema, n float64, text string) {
	switch {
	case s.Minimum != nil && n < *s.Minimum:
		x.add(path, "out of range: %s is below the minimum %s", text, fmtNum(*s.Minimum))
	case s.ExclusiveMinimum != nil && n <= *s.ExclusiveMinimum:
		x.add(path, "out of range: %s must be greater than %s", text, fmtNum(*s.ExclusiveMinimum))
	case s.Maximum != nil && n > *s.Maximum:
		x.add(path, "out of range: %s is above the maximum %s", text, fmtNum(*s.Maximum))
	case s.ExclusiveMaximum != nil && n >= *s.ExclusiveMaximum:
		x.add(path, "out of range: %s must be less than %s", text, fmtNum(*s.ExclusiveMaximum))
	case s.MultipleOf != nil && *s.MultipleOf != 0 && math.Mod(n, *s.MultipleOf) != 0:
		x.add(path, "%s is not a multiple of %s", text, fmtNum(*s.MultipleOf))
	}
}

func (x *explainer) str(path string, s *jsonschema.Schema, v string) {
	n := utf8.RuneCountInString(v)
	switch {
	case s.MinLength != nil && n < *s.MinLength:
		x.add(path, "too short: %d characters, at least %d required", n, *s.MinLength)
	case s.MaxLength != nil && n > *s.MaxLength:
		x.add(path, "too long: %d characters, at most %d allowed", n, *s.MaxLength)
	case s.Pattern != "":
		if re, err := regexp.Compile(s.Pattern); err == nil && !re.MatchString(v) {
			x.add(path, "%s does not match the pattern %q", jsonText(v), s.Pattern)
		}
	}
}

func (x *explainer) array(path string, s *jsonschema.Schema, v []any, depth int) {
	switch {
	case s.MinItems != nil && len(v) < *s.MinItems:
		x.add(path, "too few items: %d, at least %d required", len(v), *s.MinItems)
	case s.MaxItems != nil && len(v) > *s.MaxItems:
		x.add(path, "too many items: %d, at most %d allowed", len(v), *s.MaxItems)
	}
	prefix := s.PrefixItems
	if len(prefix) == 0 {
		prefix = s.ItemsArray
	}
	for i, e := range v {
		p := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case i < len(prefix):
			x.walk(p, prefix[i], e, depth+1)
		case s.Items != nil:
			x.walk(p, s.Items, e, depth+1)
		}
	}
}

func (x *explainer) object(path string, s *jsonschema.Schema, v map[string]any, depth int) {
	for _, r := range s.Required {
		if _, ok := v[r]; !ok {
			x.add(joinPath(path, r), "missing required field")
		}
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := joinPath(path, k)
		if ps, ok := s.Properties[k]; ok {
			x.walk(p, ps, v[k], depth+1)
			continue
		}
		if ps := matchPattern(s.PatternProperties, k); ps != nil {
			x.walk(p, ps, v[k], depth+1)
			continue
		}
		switch ap := x.deref(s.AdditionalProperties); {
		case s.AdditionalProperties == nil:
		case isFalseSchema(ap):
			if len(s.Properties) == 0 {
				x.add(p, "unknown field (this object takes no fields)")
			} else {
				x.add(p, "unknown field; accepted fields: %s", nameList(sortedKeys(s.Properties)))
			}
		default:
			x.walk(p, ap, v[k], depth+1)
		}
	}
}

// --- helpers ---

// validatorText strips the validator's "validating <schema>: " prefixes,
// keeping its final complaint.
func validatorText(err error) string {
	msg := unwrapMessage(err)
	for strings.HasPrefix(msg, "validating ") {
		_, rest, ok := strings.Cut(msg, ": ")
		if !ok {
			break
		}
		msg = rest
	}
	return msg
}

func isFalseSchema(s *jsonschema.Schema) bool {
	if s == nil || s.Not == nil {
		return false
	}
	b, err := json.Marshal(s.Not)
	return err == nil && (string(b) == "{}" || string(b) == "true")
}

func schemaTypes(s *jsonschema.Schema) []string {
	if s.Type != "" {
		return []string{s.Type}
	}
	return s.Types
}

func jsonTypeOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case int64:
		return "integer"
	case float64:
		if _, f := math.Modf(t); f == 0 {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func typeMatches(v any, types []string) bool {
	got := jsonTypeOf(v)
	for _, t := range types {
		if t == got || (t == "number" && got == "integer") {
			return true
		}
	}
	return false
}

// describeValue is a type name plus, for scalars, the value: `string "3"`.
func describeValue(v any) string {
	switch v.(type) {
	case map[string]any, []any, nil:
		return jsonTypeOf(v)
	}
	return jsonTypeOf(v) + " " + jsonText(v)
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return clip(string(b), maxValueText)
}

func sameValue(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

func containsValue(set []any, v any) bool {
	for _, e := range set {
		if sameValue(e, v) {
			return true
		}
	}
	return false
}

func valueList(vs []any) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = jsonText(v)
	}
	return capList(parts)
}

func nameList(names []string) string { return capList(names) }

func capList(items []string) string {
	if len(items) <= maxNamesListed {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:maxNamesListed], ", ") + fmt.Sprintf(", ... (%d more)", len(items)-maxNamesListed)
}

func sortedKeys(m map[string]*jsonschema.Schema) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func matchPattern(pp map[string]*jsonschema.Schema, k string) *jsonschema.Schema {
	for pat, s := range pp {
		if re, err := regexp.Compile(pat); err == nil && re.MatchString(k) {
			return s
		}
	}
	return nil
}

func joinPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case int64:
		return float64(t)
	case float64:
		return t
	}
	return 0
}

func fmtNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// clip cuts s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "..."
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}
