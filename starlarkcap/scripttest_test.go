package starlarkcap_test

import (
	"context"
	"os"
	"testing"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/capability-services/starlarkcap"
)

// TestScriptTests loads the testdata/scripts bundle (apk.star, a
// Starlark-defined capability that loads a shared module) and runs the
// apk_test.star it declares against it.
func TestScriptTests(t *testing.T) {
	r := baseRegistry(t)
	if _, err := starlarkcap.Load(context.Background(), r, []bundle.Source{bundle.Dir("testdata/scripts")}, starlarkcap.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot("testdata/scripts")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	starlarkcap.Test(t, r, root.FS(), starlarkcap.LoadOptions{})
}
