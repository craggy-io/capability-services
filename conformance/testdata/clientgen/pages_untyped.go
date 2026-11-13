package main

import (
	"context"
	"encoding/json"

	"conformance/client"
)

// allItems reads every page of the paged capability. The Starlark set
// declares no output schema for it, so nothing says it pages: its output
// is JSON, and following the cursor is written here.
func allItems(ctx context.Context, c client.Caller) ([]string, error) {
	var items []string
	in := client.StarItemsListIn{Limit: 3}
	for {
		raw, err := client.StarItemsList(ctx, c, in)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items []string `json:"items"`
			Next  string   `json:"next"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Next == "" {
			return items, nil
		}
		in.Cursor = page.Next
	}
}
