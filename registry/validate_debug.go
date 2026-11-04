//go:build craggy_debug

package registry

// In debug builds every result is checked against the output schema.
const debugValidateOutput = true
