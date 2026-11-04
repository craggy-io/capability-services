package registry

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxToolNameLen is the tool-name length limit shared by LLM tool-calling
// APIs and many MCP clients (pattern ^[a-zA-Z0-9_-]{1,64}$). Add enforces it
// so a capability that registers works in every projection.
const MaxToolNameLen = 64

// ValidName reports whether name is a legal capability name: dotted
// segments of [a-z0-9-], no empty segment, tool form at most 64 characters.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxToolNameLen {
		return false
	}
	for _, seg := range strings.Split(name, ".") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func validateName(name string) error {
	if !ValidName(name) {
		return Errorf(Invalid, "invalid capability name %q: segments of [a-z0-9-] joined by dots, tool form <= %d chars", name, MaxToolNameLen)
	}
	return nil
}

// ParseRef splits a reference into name and pinned version. pinned is false
// for a bare name. ok is false for a malformed ref.
func ParseRef(ref string) (name string, version int, pinned, ok bool) {
	base, ver, hasAt := strings.Cut(ref, "@")
	if !ValidName(base) {
		return "", 0, false, false
	}
	if !hasAt {
		return base, 0, false, true
	}
	v, err := strconv.Atoi(ver)
	if err != nil || v < 1 || strings.Contains(ver, "@") {
		return "", 0, false, false
	}
	return base, v, true, true
}

func refOf(name string, version int) string {
	return fmt.Sprintf("%s@%d", name, version)
}

type capKey struct {
	name    string
	version int
}

// ToolName maps a capability name to its LLM tool name: dots to underscores.
// Names cannot contain '_', so this is always reversible.
func ToolName(name string) string { return strings.ReplaceAll(name, ".", "_") }

// FromToolName is the inverse of ToolName.
func FromToolName(tool string) string { return strings.ReplaceAll(tool, "_", ".") }

// underPrefix reports whether name is prefix itself or lives under it.
func underPrefix(prefix, name string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+".")
}
