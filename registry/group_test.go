package registry_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/craggy-io/capability-services/registry"
)

func TestGroupMembershipIsLongestPrefix(t *testing.T) {
	r := registry.New()
	defer r.Close()
	mustAdd(t, r, echoCap("web.fetch"), echoCap("platform.overview"), echoCap("platform.scripts.run"), echoCap("platform.scripts"), echoCap("webhook.send"), echoCap("lone.cap"))
	if err := r.AddGroup(
		registry.Group{Name: "platform", Description: "Platform"},
		registry.Group{Name: "platform.scripts", Description: "Lab scripts", LoadAlways: true},
		registry.Group{Name: "web", Description: "Web"},
	); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"web.fetch": "web", "platform.overview": "platform",
		"platform.scripts.run": "platform.scripts", "platform.scripts": "platform.scripts",
		"webhook.send": "", "lone.cap": "",
	}
	for _, e := range r.List() {
		if e.Group != want[e.Name] {
			t.Errorf("%s: group %q, want %q", e.Name, e.Group, want[e.Name])
		}
		if d := e.Descriptor(); d.Group != e.Group {
			t.Errorf("%s: descriptor group %q, want %q", e.Name, d.Group, e.Group)
		}
	}
	if e, _ := r.Get("web.fetch"); e.Group != "web" {
		t.Errorf("Get: group %q", e.Group)
	}

	// Groups lists only groups with members in the view, sorted.
	names := func(gs []registry.Group) []string {
		var out []string
		for _, g := range gs {
			out = append(out, g.Name)
		}
		return out
	}
	if got := names(r.Groups()); !reflect.DeepEqual(got, []string{"platform", "platform.scripts", "web"}) {
		t.Fatalf("Groups = %v", got)
	}
	if got := r.View(registry.Match("platform.scripts.*")).Groups(); len(got) != 1 || got[0].Name != "platform.scripts" || !got[0].LoadAlways {
		t.Fatalf("narrowed Groups = %+v", got)
	}

	// Groups survive capability changes, and a group registered after its
	// capabilities still claims them.
	mustAdd(t, r, echoCap("web.search"))
	r.Remove("lone.cap")
	if err := r.Replace("platform", echoCap("platform.overview")); err != nil {
		t.Fatal(err)
	}
	if err := r.AddGroup(registry.Group{Name: "webhook", Description: "Webhooks"}); err != nil {
		t.Fatal(err)
	}
	if got := names(r.Groups()); !reflect.DeepEqual(got, []string{"platform", "web", "webhook"}) {
		t.Fatalf("Groups after changes = %v", got)
	}
	b, _ := json.Marshal(r.Groups()[0])
	if string(b) != `{"name":"platform","description":"Platform"}` {
		t.Fatalf("group JSON = %s", b)
	}
}

func TestAddGroupValidation(t *testing.T) {
	r := registry.New()
	defer r.Close()
	g := registry.Group{Name: "web", Description: "Web"}
	if err := r.AddGroup(g); err != nil {
		t.Fatal(err)
	}
	if err := r.AddGroup(g); err != nil {
		t.Fatalf("identical re-registration: %v", err)
	}
	for _, tc := range []struct {
		g    registry.Group
		code registry.Code
	}{
		{registry.Group{Name: "web", Description: "Other"}, registry.Conflict},
		{registry.Group{Name: "Web", Description: "x"}, registry.Invalid},
		{registry.Group{Name: "web.", Description: "x"}, registry.Invalid},
		{registry.Group{Name: "x"}, registry.Invalid},
		{registry.Group{Name: "x", Description: "two\nlines"}, registry.Invalid},
	} {
		err := r.AddGroup(tc.g)
		if registry.Info(err).Code != tc.code {
			t.Errorf("AddGroup(%+v) = %v, want %s", tc.g, err, tc.code)
		}
	}
	// A failed batch registers nothing.
	if err := r.AddGroup(registry.Group{Name: "ok", Description: "ok"}, registry.Group{Name: "bad name"}); err == nil {
		t.Fatal("bad batch accepted")
	}
	mustAdd(t, r, echoCap("ok.cap"))
	if e, _ := r.Get("ok.cap"); e.Group != "" {
		t.Fatalf("failed batch registered a group: %q", e.Group)
	}
}
