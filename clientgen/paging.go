package clientgen

import (
	"fmt"
	"strings"
)

// paging is how a capability that returns its result a page at a time is
// read to the end.
type paging struct {
	cursor  bool   // true: cursor in, next out. false: offset in, eof out.
	control string // the input field that says where a page starts: Cursor or Offset
	done    string // the output field that says there is no further page: Next or Eof
	page    field  // the output field that holds a page's items or bytes
}

// paging detects a paged capability from the Go types of its input and
// output. Both must be structs, and one of these must hold:
//
//   - cursor paging, the library's convention (registry.PageIn and Page):
//     the input has a string property "cursor" and the output a string
//     property "next", empty on the last page;
//   - offset paging: the input has an integer property "offset" and the
//     output a boolean property "eof". The offset counts what a page
//     holds: items, or bytes.
//
// In both, the output must have exactly one property that is an array or
// a base64 string, which is the page; when it has several, the one named
// "items" is the page, and without one the capability is not paged.
func (g *generator) paging(in, out string) *paging {
	inFields, outFields := g.structs[in], g.structs[out]
	find := func(fields []field, name string, k kind) *field {
		for i, f := range fields {
			if f.jsonName == name && f.typ.kind == k && !f.typ.pointer {
				return &fields[i]
			}
		}
		return nil
	}
	p := &paging{}
	if c, n := find(inFields, "cursor", kindString), find(outFields, "next", kindString); c != nil && n != nil {
		p.cursor, p.control, p.done = true, c.goName, n.goName
	} else if o, e := find(inFields, "offset", kindInt), find(outFields, "eof", kindBool); o != nil && e != nil {
		p.control, p.done = o.goName, e.goName
	} else {
		return nil
	}
	var pages []field
	for _, f := range outFields {
		if f.typ.kind == kindSlice || f.typ.kind == kindBytes {
			pages = append(pages, f)
		}
	}
	if len(pages) > 1 {
		named := pages[:0:0]
		for _, f := range pages {
			if f.jsonName == "items" {
				named = append(named, f)
			}
		}
		pages = named
	}
	if len(pages) != 1 {
		return nil
	}
	p.page = pages[0]
	return p
}

// source is the two functions generated for a paged capability.
func (p *paging) source(name, ref string) string {
	var b strings.Builder
	what, joined := "items", "in order"
	if p.page.typ.kind == kindBytes {
		what, joined = "bytes", "joined"
	}
	var doc string
	if p.cursor {
		doc = fmt.Sprintf("%sPages calls %s page after page and yields each page's output: it starts at in.%s (empty for the first page) and follows each output's %s until one is empty. A page whose %s is the cursor it was asked with is an error, not a page to ask for again.",
			name, ref, p.control, p.done, p.done)
	} else {
		doc = fmt.Sprintf("%sPages calls %s page after page and yields each page's output: it starts at in.%s and moves on by the number of %s in each output's %s, until an output has %s set or holds none.",
			name, ref, p.control, what, p.page.goName, p.done)
	}
	comment(&b, "", doc+" The other fields of in are sent unchanged with every call, the page size among them. A call that fails is yielded as its error, with a zero output, and ends the sequence; so does the caller breaking out of the loop.")
	fmt.Fprintf(&b, "func %sPages(ctx context.Context, c Caller, in %sIn) iter.Seq2[%sOut, error] {\n", name, name, name)
	fmt.Fprintf(&b, "\treturn func(yield func(%sOut, error) bool) {\n\t\tfor {\n", name)
	fmt.Fprintf(&b, "\t\t\tout, err := %s(ctx, c, in)\n\t\t\tif err != nil {\n\t\t\t\tyield(%sOut{}, err)\n\t\t\t\treturn\n\t\t\t}\n", name, name)
	if p.cursor {
		fmt.Fprintf(&b, "\t\t\tif !yield(out, nil) || out.%s == \"\" {\n\t\t\t\treturn\n\t\t\t}\n", p.done)
		fmt.Fprintf(&b, "\t\t\tif out.%s == in.%s {\n\t\t\t\tyield(%sOut{}, fmt.Errorf(\"%%s: the next cursor is the one the page was asked with\", %sRef))\n\t\t\t\treturn\n\t\t\t}\n", p.done, p.control, name, name)
		fmt.Fprintf(&b, "\t\t\tin.%s = out.%s\n", p.control, p.done)
	} else {
		fmt.Fprintf(&b, "\t\t\tif !yield(out, nil) || out.%s || len(out.%s) == 0 {\n\t\t\t\treturn\n\t\t\t}\n", p.done, p.page.goName)
		fmt.Fprintf(&b, "\t\t\tin.%s += int64(len(out.%s))\n", p.control, p.page.goName)
	}
	b.WriteString("\t\t}\n\t}\n}\n\n")

	comment(&b, "", fmt.Sprintf("%sAll reads every page of %s with %sPages and returns the %s of every page's %s, %s. It holds them all in memory: range over %sPages to stop early or to handle a page at a time.",
		name, ref, name, what, p.page.goName, joined, name))
	fmt.Fprintf(&b, "func %sAll(ctx context.Context, c Caller, in %sIn) (%s, error) {\n", name, name, p.page.typ.expr)
	fmt.Fprintf(&b, "\tvar all %s\n\tfor out, err := range %sPages(ctx, c, in) {\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n", p.page.typ.expr, name)
	fmt.Fprintf(&b, "\t\tall = append(all, out.%s...)\n\t}\n\treturn all, nil\n}\n\n", p.page.goName)
	return b.String()
}
