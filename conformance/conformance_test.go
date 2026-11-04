package conformance_test

import (
	"testing"

	"github.com/craggy-io/capability-services/conformance"
)

func TestConformanceSuite(t *testing.T) {
	conformance.Suite(t)
}
