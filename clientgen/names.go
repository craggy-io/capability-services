package clientgen

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// pascal turns a capability name, a property name or an enum value into
// the exported part of a Go identifier: the text is cut at every character
// that is not a letter or a digit, the first letter of each piece is made
// upper case, and the pieces are joined. Nothing else changes, so
// "artifacts.runs.record-tree" is ArtifactsRunsRecordTree and "artifactId"
// is ArtifactId. A result that would start with a digit gets an X in
// front; a text with no letter or digit gives "".
func pascal(text string) string {
	var b strings.Builder
	upper := true
	for _, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if r, _ := utf8.DecodeRuneInString(out); unicode.IsDigit(r) {
		return "X" + out
	}
	return out
}

// taggable reports whether encoding/json accepts name as the name in a
// struct tag. It refuses a quote, a backslash and a comma, among others,
// and then ignores the whole tag.
func taggable(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", r):
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			return false
		}
	}
	return true
}

// jsonTag is the struct tag of a property.
func jsonTag(name string, omitEmpty bool) string {
	tag := name
	switch {
	case omitEmpty:
		tag += ",omitempty"
	case name == "-":
		tag += "," // "-" alone would mean: skip the field
	}
	return "`json:\"" + tag + "\"`"
}

// goFile is the file a namespace's capabilities go in. The name ends in
// _client so that no namespace can turn it into a file the go tool builds
// only for one system, or as a test.
func goFile(namespace string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(namespace) + "_client.go"
}

// namespaceOf is the first segment of a capability name.
func namespaceOf(name string) string {
	ns, _, _ := strings.Cut(name, ".")
	return ns
}

// comment writes text as a // comment, wrapped at width columns. Line
// breaks in the text are kept.
func comment(b *strings.Builder, indent, text string) {
	const width = 76
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		words := strings.Fields(line)
		if len(words) == 0 {
			b.WriteString(indent + "//\n")
			continue
		}
		cur := ""
		for _, w := range words {
			if cur != "" && utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) > width-len(indent) {
				b.WriteString(indent + "// " + cur + "\n")
				cur = ""
			}
			if cur != "" {
				cur += " "
			}
			cur += w
		}
		b.WriteString(indent + "// " + cur + "\n")
	}
}
