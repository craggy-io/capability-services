package capabilityservices_test

import (
	"testing"

	"github.com/craggy-io/foundation-kit/archtest"
)

// projections are the packages rules 1 and 2 of the AGENTS.md import rules call
// projections.
var projections = []string{"./jobs", "./tools", "./mcp", "./httpapi", "./starlarkcap"}

// noOtherProjection returns rule 2 for one projection: it imports no other
// projection, except those listed in allow.
func noOtherProjection(from string, allow ...string) archtest.Rule {
	return archtest.Rule{
		Name:   "rule 2: " + from + " imports no other projection",
		Why:    "projections import registry and never each other, except mcp, which is built on tools (AGENTS.md, Import rules 2)",
		From:   []string{from},
		Forbid: projections,
		Allow:  append([]string{from}, allow...),
	}
}

// TestArchitecture enforces rules 1-3 and 6 of the "Import rules" in AGENTS.md.
// archtest skips _test.go files, so the test-file part of rule 3 stays in the
// AGENTS.md audit. Rule 4 is checked in each domain module, and rule 5 is not an
// import rule.
func TestArchitecture(t *testing.T) {
	archtest.Check(t, archtest.Config{
		Rules: []archtest.Rule{
			{
				// Forbidding every external module covers "any domain module" too.
				Name:   "rule 1: registry is stdlib plus jsonschema-go",
				Why:    "core registry imports the standard library and github.com/google/jsonschema-go only, never a projection or domain module (AGENTS.md, Import rules 1)",
				From:   []string{"./registry"},
				Forbid: []string{archtest.External, "./..."},
				Allow:  []string{"github.com/google/jsonschema-go/...", "./registry"},
			},
			noOtherProjection("./jobs"),
			noOtherProjection("./tools"),
			noOtherProjection("./mcp", "./tools"),
			noOtherProjection("./httpapi"),
			noOtherProjection("./starlarkcap"),
			{
				Name:   "rule 3: only starlarkcap imports script-services",
				Why:    "use the starlarkcap aliases (Position, Limits, Frame) instead (AGENTS.md, Import rules 3)",
				From:   []string{"./..."},
				Except: []string{"./starlarkcap"},
				Forbid: []string{"github.com/craggy-io/script-services/..."},
			},
			{
				// Hosts outside this module can't be named here; the example
				// host is the one this module holds.
				Name:   "rule 6: nothing imports the host",
				Why:    "no library imports the application that wires it (AGENTS.md, Import rules 6)",
				From:   []string{"./..."},
				Except: []string{"./examples/host"},
				Forbid: []string{"./examples/host"},
			},
		},
	})
}
