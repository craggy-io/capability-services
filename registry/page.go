package registry

import (
	"encoding/base64"
	"strconv"
)

// PageIn is the one pagination input convention. Inputs embed it.
type PageIn struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"max items; default 50, max 500"`
	Cursor string `json:"cursor,omitempty" jsonschema:"opaque, from a previous next"`
}

// Page is the one pagination output convention. Next is empty on the last
// page; following it visits each item exactly once.
type Page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next,omitempty"`
}

const (
	DefaultPageLimit = 50
	MaxPageLimit     = 500
)

// NormalizeLimit clamps a requested limit to [1, MaxPageLimit], with 0 or
// negative meaning the default.
func NormalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultPageLimit
	}
	if limit > MaxPageLimit {
		return MaxPageLimit
	}
	return limit
}

func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, Errorf(Invalid, "invalid cursor")
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0, Errorf(Invalid, "invalid cursor")
	}
	return n, nil
}

// Paginate slices items (already in a stable order) according to in. The
// cursor is opaque to callers.
func Paginate[T any](items []T, in PageIn) (Page[T], error) {
	off, err := decodeCursor(in.Cursor)
	if err != nil {
		return Page[T]{}, err
	}
	if off > len(items) {
		return Page[T]{}, Errorf(Invalid, "cursor past end of results")
	}
	limit := NormalizeLimit(in.Limit)
	end := off + limit
	if end > len(items) {
		end = len(items)
	}
	page := Page[T]{Items: append([]T(nil), items[off:end]...)}
	if page.Items == nil {
		page.Items = []T{}
	}
	if end < len(items) {
		page.Next = encodeCursor(end)
	}
	return page, nil
}
