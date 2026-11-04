package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/craggy-io/capability-services/registry"
)

// openapi generates an OpenAPI 3.1 document on each request, so runtime
// registration changes show up immediately. It includes bare paths for
// current versions plus a pinned path for every registered version. Every
// operation takes the optional Craggy-Session request header, and
// capability results may carry the Craggy-Superseded-By response header.
func (h *handler) openapi(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.openAPIDoc())
}

func (h *handler) openAPIDoc() map[string]any {
	paths := map[string]any{}
	add := func(path string, e registry.Entry, pinned bool) {
		paths[path] = map[string]any{"post": operation(e, pinned)}
	}
	for _, e := range h.v.All() {
		base := "/v1/" + pathOf(e.Name)
		add(base+"@"+strconv.Itoa(e.Version), e, true)
		if e.Current {
			add(base, e, false)
		}
	}
	paths["/v1/"] = getOp("List capabilities (?all=1 for everything)")
	paths["/v1/health"] = getOp("Capability readiness")
	paths["/v1/jobs/{id}"] = map[string]any{
		"get":    opSummary("Get a job's status (plus result on success)"),
		"delete": opSummary("Cancel a job"),
	}
	paths["/v1/jobs/{id}/events"] = getOp("Stream job events as Server-Sent Events")
	for _, ops := range paths {
		for _, op := range ops.(map[string]any) {
			op.(map[string]any)["parameters"] = []any{map[string]string{"$ref": "#/components/parameters/CraggySession"}}
		}
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]string{"title": h.cfg.title, "version": h.cfg.ver},
		"paths":   paths,
		"components": map[string]any{
			"parameters": map[string]any{
				"CraggySession": map[string]any{
					"name":        sessionHeader,
					"in":          "header",
					"required":    false,
					"description": "Caller session: sets Caller.Session for this request, overriding the host's caller function. Job access defaults to jobs from the caller's own session.",
					"schema":      map[string]string{"type": "string"},
				},
			},
			"headers": map[string]any{
				"CraggySupersededBy": map[string]any{
					"description": "Set when the capability called is superseded: the comma-separated names (or name@version refs) that supersede it.",
					"schema":      map[string]string{"type": "string"},
				},
			},
			"schemas": map[string]any{
				"Error": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"error": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"code": map[string]any{"type": "string"},
								"reason": map[string]any{
									"type":        "string",
									"description": "optional stable lower_snake_case refinement of code, e.g. no_capacity",
								},
								"message": map[string]any{"type": "string"},
								"retryable": map[string]any{
									"type":        "boolean",
									"description": "true when the same call may succeed if made again later (code unavailable)",
								},
							},
							"required": []string{"code", "message", "retryable"},
						},
					},
					"required": []string{"error"},
				},
			},
		},
	}
}

func schemaOrEmpty(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	return raw
}

func operation(e registry.Entry, pinned bool) map[string]any {
	cj := e.Descriptor()
	opID := registry.ToolName(e.Name)
	if pinned {
		opID += "_v" + strconv.Itoa(e.Version)
	}
	responses := map[string]any{
		"400": errResp("Invalid input"),
		"403": errResp("Denied"),
		"404": errResp("Not found in this view"),
		"409": errResp("Conflict"),
		"499": errResp("Cancelled"),
		"503": errResp("Capability not ready"),
		"500": errResp("Internal error"),
	}
	supersededBy := map[string]any{supersededHeader: map[string]string{"$ref": "#/components/headers/CraggySupersededBy"}}
	ok := map[string]any{
		"description": "Result",
		"headers":     supersededBy,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"result": schemaOrEmpty(cj.Output)},
					"required":   []string{"result"},
				},
			},
		},
	}
	if e.Long {
		responses["202"] = map[string]any{
			"description": "Started as a job; follow Location. Send Prefer: wait=N to wait up to N seconds first.",
			"headers": map[string]any{
				"Location":       map[string]any{"schema": map[string]any{"type": "string"}},
				supersededHeader: supersededBy[supersededHeader],
			},
		}
	}
	responses["200"] = ok
	desc := e.Description
	if note := registry.DescriptionNotes(e.Capability); note != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += note
	}
	op := map[string]any{
		"operationId": opID,
		"summary":     e.Title,
		"description": desc,
		"requestBody": map[string]any{
			"content": map[string]any{
				"application/json": map[string]any{"schema": schemaOrEmpty(cj.Input)},
			},
		},
		"responses": responses,
	}
	if e.Superseded() {
		op["deprecated"] = true
	}
	if len(e.Tags) > 0 {
		op["tags"] = e.Tags
	}
	return op
}

func errResp(desc string) map[string]any {
	return map[string]any{
		"description": desc,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]string{"$ref": "#/components/schemas/Error"},
			},
		},
	}
}

func opSummary(s string) map[string]any {
	return map[string]any{"summary": s, "responses": map[string]any{"200": map[string]any{"description": "OK"}}}
}

func getOp(s string) map[string]any { return map[string]any{"get": opSummary(s)} }
