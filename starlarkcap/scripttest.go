package starlarkcap

import (
	"io/fs"
	"testing"

	"github.com/craggy-io/script-services/startest"

	"github.com/craggy-io/capability-services/registry"
)

// Test runs every "*_test.star" file in fsys as Go subtests of t (see
// startest.Run). Test files and the modules they load see `assert` plus
// the capability globals (namespaces, call(), jobs.start) of
// r.View(registry.All), so they can call any capability in r, including
// ones a capability file defined through Load. Modules load from fsys. For
// a directory on disk pass an os.Root's FS (or modules.DirRoot's), which
// keeps symbolic links inside it; os.DirFS follows them out.
//
//	func TestCapabilities(t *testing.T) {
//		r := registry.New()
//		defer r.Close()
//		// ... add Go capabilities, Load the .star files under test ...
//		root, err := os.OpenRoot("testdata")
//		if err != nil {
//			t.Fatal(err)
//		}
//		defer root.Close()
//		starlarkcap.Test(t, r, root.FS())
//	}
func Test(t *testing.T, r *registry.Registry, fsys fs.FS) {
	t.Helper()
	ctx := registry.WithView(t.Context(), r.View(registry.All))
	startest.Run(t, fsys, startest.Options{Predeclared: Globals(ctx)})
}
