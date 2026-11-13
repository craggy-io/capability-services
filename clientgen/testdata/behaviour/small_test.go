// This file is compiled and run beside the client generated from
// testdata/small.json, in a module that requires nothing (see
// TestGeneratedCodeBehaves). It is under testdata so that the go tool
// never builds it where it lies.
package small

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// fake is a Caller that answers from a function and records what it was
// sent, as the JSON a real one would put on the wire.
type fake struct {
	answer func(ref string, in map[string]any) (any, error)
	refs   []string
	sent   []string
}

func (f *fake) Call(_ context.Context, ref string, in any) (json.RawMessage, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	f.refs, f.sent = append(f.refs, ref), append(f.sent, string(raw))
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("input is not an object: %s", raw)
	}
	out, err := f.answer(ref, fields)
	if err != nil {
		return nil, err
	}
	if raw, ok := out.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(out)
}

// The hand-written equivalent of ShopOrdersCreateIn: what a person would
// write for the same schema.
type orderIn struct {
	Customer   string            `json:"customer"`
	Items      []orderItem       `json:"items"`
	Priority   string            `json:"priority,omitempty"`
	Shipping   *shipping         `json:"shipping,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Attachment []byte            `json:"attachment,omitempty"`
	Metadata   json.RawMessage   `json:"metadata,omitempty"`
	Discount   *float64          `json:"discount"`
	Gift       bool              `json:"gift,omitempty"`
}

type orderItem struct {
	Sku      string `json:"sku"`
	Quantity int64  `json:"quantity"`
	Note     string `json:"note,omitempty"`
}

type shipping struct {
	Address    string `json:"address"`
	PostalCode string `json:"postal-code,omitempty"`
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A generated type marshals to the bytes a hand-written struct with the
// same tags does: full, and empty.
func TestSameJSONAsHandWritten(t *testing.T) {
	discount := 2.5
	gen := ShopOrdersCreateIn{
		Customer: "c-1",
		Items:    []ShopOrdersCreateInItemsItem{{Sku: "a", Quantity: 2, Note: "fragile"}, {Sku: "b", Quantity: 1}},
		Priority: ShopOrdersCreateInPriorityHigh,
		Shipping: &ShopOrdersCreateInShipping{Address: "1 Main St", PostalCode: "AB1"},
		Tags:     map[string]string{"k": "v"}, Attachment: []byte{0, 1, 2, 250},
		Metadata: json.RawMessage(`{"n":1e3,"big":9007199254740993}`), Discount: &discount, Gift: true,
	}
	hand := orderIn{
		Customer: "c-1",
		Items:    []orderItem{{Sku: "a", Quantity: 2, Note: "fragile"}, {Sku: "b", Quantity: 1}},
		Priority: "high",
		Shipping: &shipping{Address: "1 Main St", PostalCode: "AB1"},
		Tags:     map[string]string{"k": "v"}, Attachment: []byte{0, 1, 2, 250},
		Metadata: json.RawMessage(`{"n":1e3,"big":9007199254740993}`), Discount: &discount, Gift: true,
	}
	if g, h := mustJSON(t, gen), mustJSON(t, hand); g != h {
		t.Errorf("full value:\ngenerated    %s\nhand-written %s", g, h)
	}
	want := `{"customer":"","items":null,"discount":null}`
	if g, h := mustJSON(t, ShopOrdersCreateIn{}), mustJSON(t, orderIn{}); g != h || g != want {
		t.Errorf("zero value:\ngenerated    %s\nhand-written %s\nwant         %s", g, h, want)
	}
}

// JSON read into a generated type and written again is what the
// hand-written struct gives: members the type does not have are dropped
// by both, and any-JSON members come back as they were sent.
func TestRoundTrip(t *testing.T) {
	doc := `{"customer":"c-1","unknown":{"x":1},"items":[{"sku":"a","quantity":3,"later":true}],
		"shipping":{"address":"1 Main St"},"attachment":"AAEC+g==","metadata":[1.50,"two",{"three":3}],"discount":null}`
	var gen ShopOrdersCreateIn
	var hand orderIn
	if err := json.Unmarshal([]byte(doc), &gen); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(doc), &hand); err != nil {
		t.Fatal(err)
	}
	g, h := mustJSON(t, gen), mustJSON(t, hand)
	if g != h {
		t.Errorf("round trip:\ngenerated    %s\nhand-written %s", g, h)
	}
	if !bytes.Equal(gen.Attachment, []byte{0, 1, 2, 250}) {
		t.Errorf("base64 text became %v", gen.Attachment)
	}
	if !strings.Contains(g, `"metadata":[1.50,"two",{"three":3}]`) {
		t.Errorf("any-JSON member was rewritten: %s", g)
	}
}

func TestCall(t *testing.T) {
	ctx := context.Background()
	f := &fake{answer: func(ref string, in map[string]any) (any, error) {
		return json.RawMessage(`{"id":"o-1","status":"paid","total":12.5,"createdAt":"2026-01-02T03:04:05Z","addedLater":[1,2]}`), nil
	}}
	out, err := ShopOrdersCreate(ctx, f, ShopOrdersCreateIn{Customer: "c-1", Items: []ShopOrdersCreateInItemsItem{{Sku: "a", Quantity: 1}}})
	if err != nil {
		t.Fatal(err) // a member the output type lacks is ignored
	}
	if f.refs[0] != "shop.orders.create@1" || ShopOrdersCreateRef != "shop.orders.create@1" {
		t.Errorf("called %v", f.refs)
	}
	if want := `{"customer":"c-1","items":[{"sku":"a","quantity":1}],"discount":null}`; f.sent[0] != want {
		t.Errorf("sent %s, want %s", f.sent[0], want)
	}
	if out.Id != "o-1" || out.Status != ShopOrdersCreateOutStatusPaid || out.Total != 12.5 || out.CreatedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("output = %+v", out)
	}

	// The Caller's error is the function's error, unwrapped.
	boom := errors.New("conflict: boom")
	f = &fake{answer: func(string, map[string]any) (any, error) { return nil, boom }}
	if _, err := ShopOrdersCreate(ctx, f, ShopOrdersCreateIn{}); err != boom {
		t.Errorf("error = %v, want the Caller's own", err)
	}
	// An output that does not fit the type is an error that names the capability.
	f = &fake{answer: func(string, map[string]any) (any, error) { return json.RawMessage(`{"total":"much"}`), nil }}
	if _, err := ShopOrdersCreate(ctx, f, ShopOrdersCreateIn{}); err == nil || !strings.Contains(err.Error(), "shop.orders.create@1: decoding the output") {
		t.Errorf("error = %v", err)
	}
}

func TestRootsThatAreNotStructs(t *testing.T) {
	ctx := context.Background()
	// No input schema: the empty object. No output schema: the JSON as it came.
	f := &fake{answer: func(string, map[string]any) (any, error) { return json.RawMessage(`{"pong":true}`), nil }}
	raw, err := ShopPingV1(ctx, f, ShopPingV1In{})
	if err != nil || string(raw) != `{"pong":true}` || f.sent[0] != `{}` || f.refs[0] != ShopPingV1Ref {
		t.Errorf("ping@1 = %s, %v; sent %v to %v", raw, err, f.sent, f.refs)
	}
	// A Caller that returns nothing at all leaves the output zero.
	f = &fake{answer: func(string, map[string]any) (any, error) { return json.RawMessage(nil), nil }}
	if raw, err := ShopPingV1(ctx, f, ShopPingV1In{}); err != nil || raw != nil {
		t.Errorf("empty output = %s, %v", raw, err)
	}
	// A map input that is nil is sent as the empty object, never null.
	f = &fake{answer: func(string, map[string]any) (any, error) { return []string{"a", "b"}, nil }}
	names, err := ShopPingV2(ctx, f, nil)
	if err != nil || !reflect.DeepEqual(names, []string{"a", "b"}) || f.sent[0] != `{}` || f.refs[0] != "shop.ping@2" {
		t.Errorf("ping@2 = %v, %v; sent %v to %v", names, err, f.sent, f.refs)
	}
}

// orders is a Caller that pages a list of n orders by the library's
// convention: limit and cursor in, items and next out.
func orders(n int) *fake {
	return &fake{answer: func(ref string, in map[string]any) (any, error) {
		start := 0
		if c, ok := in["cursor"].(string); ok {
			start, _ = strconv.Atoi(c)
		}
		limit := 50
		if l, ok := in["limit"].(float64); ok {
			limit = int(l)
		}
		out := map[string]any{}
		items := []map[string]any{}
		for i := start; i < n && i < start+limit; i++ {
			items = append(items, map[string]any{"id": fmt.Sprint("o-", i), "total": float64(i)})
		}
		out["items"] = items
		if start+limit < n {
			out["next"] = strconv.Itoa(start + limit)
		}
		return out, nil
	}}
}

func TestCursorPaging(t *testing.T) {
	ctx := context.Background()
	f := orders(7)
	all, err := ShopOrdersListAll(ctx, f, ShopOrdersListIn{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 7 || all[0].Id != "o-0" || all[6].Id != "o-6" {
		t.Errorf("all = %+v", all)
	}
	// Each page is asked for with the cursor the one before gave, and
	// the page size the caller chose.
	if want := []string{`{"limit":3}`, `{"limit":3,"cursor":"3"}`, `{"limit":3,"cursor":"6"}`}; !reflect.DeepEqual(f.sent, want) {
		t.Errorf("sent %v, want %v", f.sent, want)
	}
	// A list that fits one page is one call; an empty list is one call and no items.
	f = orders(2)
	if all, err := ShopOrdersListAll(ctx, f, ShopOrdersListIn{}); err != nil || len(all) != 2 || len(f.sent) != 1 {
		t.Errorf("short list: %v, %v after %d calls", all, err, len(f.sent))
	}
	f = orders(0)
	if all, err := ShopOrdersListAll(ctx, f, ShopOrdersListIn{}); err != nil || len(all) != 0 || len(f.sent) != 1 {
		t.Errorf("empty list: %v, %v after %d calls", all, err, len(f.sent))
	}
	// Reading starts at the cursor given.
	f = orders(7)
	if all, err := ShopOrdersListAll(ctx, f, ShopOrdersListIn{Limit: 3, Cursor: "3"}); err != nil || len(all) != 4 || all[0].Id != "o-3" {
		t.Errorf("from a cursor: %v, %v", all, err)
	}
	// Breaking out of the pages stops the calls.
	f = orders(7)
	for page, err := range ShopOrdersListPages(ctx, f, ShopOrdersListIn{Limit: 3}) {
		if err != nil || len(page.Items) != 3 {
			t.Fatalf("page = %+v, %v", page, err)
		}
		break
	}
	if len(f.sent) != 1 {
		t.Errorf("%d calls after breaking at the first page", len(f.sent))
	}
	// A failed call ends the pages with its error, and All returns nothing.
	boom := errors.New("unavailable: down")
	calls := 0
	failing := orders(7)
	inner := failing.answer
	failing.answer = func(ref string, in map[string]any) (any, error) {
		if calls++; calls == 2 {
			return nil, boom
		}
		return inner(ref, in)
	}
	if all, err := ShopOrdersListAll(ctx, failing, ShopOrdersListIn{Limit: 3}); err != boom || all != nil {
		t.Errorf("failing: %v, %v", all, err)
	}
	// A provider that keeps giving the same cursor is an error, not a loop.
	stuck := &fake{answer: func(string, map[string]any) (any, error) {
		return map[string]any{"items": []any{}, "next": "same"}, nil
	}}
	if _, err := ShopOrdersListAll(ctx, stuck, ShopOrdersListIn{}); err == nil || !strings.Contains(err.Error(), "next cursor") || len(stuck.sent) != 2 {
		t.Errorf("stuck: %v after %d calls", err, len(stuck.sent))
	}
}

// blob is a Caller that serves a window of data: offset and limit in,
// data and eof out. It returns at most most bytes a call.
func blob(data []byte, most int) *fake {
	return &fake{answer: func(ref string, in map[string]any) (any, error) {
		offset, _ := in["offset"].(float64)
		limit, ok := in["limit"].(float64)
		if !ok || int(limit) > most {
			limit = float64(most)
		}
		start := min(int(offset), len(data))
		end := min(start+int(limit), len(data))
		return map[string]any{"data": data[start:end], "eof": end == len(data), "total": len(data)}, nil
	}}
}

func TestOffsetPaging(t *testing.T) {
	ctx := context.Background()
	data := make([]byte, 150_000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	f := blob(data, 65536)
	got, err := MediaBlobReadAll(ctx, f, MediaBlobReadIn{Id: "b-1", Limit: 65536})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
	want := []string{`{"id":"b-1","limit":65536}`, `{"id":"b-1","offset":65536,"limit":65536}`, `{"id":"b-1","offset":131072,"limit":65536}`}
	if !reflect.DeepEqual(f.sent, want) {
		t.Errorf("sent %v, want %v", f.sent, want)
	}
	// Reading starts at the offset given.
	f = blob(data, 65536)
	if got, err := MediaBlobReadAll(ctx, f, MediaBlobReadIn{Id: "b-1", Offset: 149_990}); err != nil || !bytes.Equal(got, data[149_990:]) {
		t.Errorf("from an offset: %d bytes, %v", len(got), err)
	}
	// Nothing to read is one call.
	f = blob(nil, 65536)
	if got, err := MediaBlobReadAll(ctx, f, MediaBlobReadIn{Id: "b-1"}); err != nil || len(got) != 0 || len(f.sent) != 1 {
		t.Errorf("empty: %d bytes, %v after %d calls", len(got), err, len(f.sent))
	}
	// A provider that returns no bytes without saying eof ends the read.
	stuck := &fake{answer: func(string, map[string]any) (any, error) {
		return map[string]any{"data": []byte{}, "eof": false, "total": 9}, nil
	}}
	if got, err := MediaBlobReadAll(ctx, stuck, MediaBlobReadIn{Id: "b-1"}); err != nil || len(got) != 0 || len(stuck.sent) != 1 {
		t.Errorf("no progress: %d bytes, %v after %d calls", len(got), err, len(stuck.sent))
	}
	// The pages are there for a caller that wants a bound of its own.
	f = blob(data, 65536)
	var head []byte
	for page, err := range MediaBlobReadPages(ctx, f, MediaBlobReadIn{Id: "b-1", Limit: 65536}) {
		if err != nil {
			t.Fatal(err)
		}
		if head = append(head, page.Data...); len(head) >= 100_000 {
			break
		}
	}
	if len(head) != 131072 || len(f.sent) != 2 {
		t.Errorf("bounded read: %d bytes after %d calls", len(head), len(f.sent))
	}
}

// Named definitions, recursion, enums, and members with no single Go
// type: a tree read through the client is the tree that was sent.
func TestTree(t *testing.T) {
	sent := `{"name":"root","kind":"folder","children":[{"name":"a.txt","kind":"file","link":"../a","2d":true,` +
		`"extra":{"big":9007199254740993,"nested":{"n":18446744073709551615},"ratio":0.1},"sizes":{"bytes":12},"owner":"ada"},` +
		`{"name":"b","kind":"folder","link":{"to":"elsewhere"},"owner":null}],"parent":{"name":"up","kind":"folder"}}`
	f := &fake{answer: func(string, map[string]any) (any, error) { return json.RawMessage(sent), nil }}
	tree, err := MediaTreeGet(context.Background(), f, MediaTreeGetIn{Root: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if f.sent[0] != `{"root":"/"}` {
		t.Errorf("sent %s", f.sent[0])
	}
	a, b := tree.Children[0], tree.Children[1]
	if tree.Kind != MediaTreeGetOutKindFolder || a.Kind != MediaTreeGetOutKindFile || tree.Parent == nil || tree.Parent.Name != "up" {
		t.Errorf("tree = %+v", tree)
	}
	if string(a.Link) != `"../a"` || string(b.Link) != `{"to":"elsewhere"}` || !a.X2d || a.Sizes["bytes"] != 12 {
		t.Errorf("a = %+v, b = %+v", a, b)
	}
	if a.Owner == nil || *a.Owner != "ada" || b.Owner != nil {
		t.Errorf("owners = %v, %v", a.Owner, b.Owner)
	}
	// A number in an any is the number that was sent, however large.
	if big, ok := a.Extra["big"].(json.Number); !ok || big.String() != "9007199254740993" {
		t.Errorf("big = %#v", a.Extra["big"])
	}
	// Written again, it is the JSON that was sent ("owner": null is an
	// absent owner: the property is optional).
	want := strings.Replace(sent, `,"owner":null`, ``, 1)
	if got := mustJSON(t, tree); got != want {
		t.Errorf("round trip:\n got %s\nwant %s", got, want)
	}
}
