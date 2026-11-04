package starlarkcap_test

import (
	"context"
	"io/fs"
	"testing"

	"github.com/craggy-io/script-services/modules"

	"github.com/craggy-io/capability-services/starlarkcap"
)

// TestScriptTests loads testdata/scripts/apk.star (a Starlark-defined
// capability that loads a shared module) and runs apk_test.star against it.
func TestScriptTests(t *testing.T) {
	r := baseRegistry(t)
	root, err := modules.DirRoot("testdata/scripts")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	src, err := fs.ReadFile(root.FS(), "apk.star")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := starlarkcap.Load(context.Background(), r, "apk", starlarkcap.Source{
		Filename: "apk.star", Src: src, Modules: root,
	}, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	starlarkcap.Test(t, r, root.FS())
}
