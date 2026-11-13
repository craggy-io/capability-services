package main

import (
	"context"

	"conformance/client"
)

// allItems reads every page of the paged capability. The Go-registered
// set declares its output (a registry.Page), so the generated client has
// the function that follows the cursor.
func allItems(ctx context.Context, c client.Caller) ([]string, error) {
	return client.TestItemsListAll(ctx, c, client.TestItemsListIn{Limit: 3})
}
