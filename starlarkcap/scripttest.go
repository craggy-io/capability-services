package starlarkcap

import (
	"io/fs"
	"testing"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/script-services/startest"

	"github.com/craggy-io/capability-services/registry"
)

// Test loads the bundle rooted at fsys (it must hold bundle.yaml) and runs
// every craggy.script-test@1 contribution it declares as Go subtests of t
// (see startest.Run). Undeclared *_test.star files do not run. Test files
// and the modules they load see `assert` plus the capability globals
// (namespaces, call(), jobs.start) of r.View(registry.All), so they can
// call any capability in r, including ones a capability file installed
// through Load. The bundle's capability files are compiled with opts, as
// Load would, but not installed: install them into r first. For a
// directory on disk pass an os.Root's FS, which keeps symbolic links
// inside it; os.DirFS follows them out.
//
//	func TestCapabilities(t *testing.T) {
//		r := registry.New()
//		defer r.Close()
//		// ... add Go capabilities ...
//		if _, err := starlarkcap.Load(t.Context(), r, []bundle.Source{bundle.Dir("testdata/caps")}, starlarkcap.LoadOptions{}); err != nil {
//			t.Fatal(err)
//		}
//		root, err := os.OpenRoot("testdata/caps")
//		if err != nil {
//			t.Fatal(err)
//		}
//		defer root.Close()
//		starlarkcap.Test(t, r, root.FS(), starlarkcap.LoadOptions{})
//	}
func Test(t *testing.T, r *registry.Registry, fsys fs.FS, opts LoadOptions) {
	t.Helper()
	ctx := registry.WithView(t.Context(), r.View(registry.All))
	startest.Run(t, fsys, startest.Options{
		Predeclared: Globals(ctx),
		Compilers:   []bundle.AnyCompiler{compiler(opts)},
	})
}
