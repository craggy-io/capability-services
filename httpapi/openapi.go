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
	jobIDParam := map[string]string{"$ref": "#/components/parameters/JobId"}
	jobEnvelopeContent := map[string]any{
		"application/json": map[string]any{
			"schema": map[string]string{"$ref": "#/components/schemas/JobEnvelope"},
		},
	}
	jobOp := func(summary string) map[string]any {
		return map[string]any{
			"summary":    summary,
			"parameters": []any{jobIDParam},
			"responses": map[string]any{
				"200": map[string]any{"description": "Job envelope", "content": jobEnvelopeContent},
				"404": errResp("Job not found (or not visible to this caller)"),
			},
		}
	}
	// merge, never overwrite: a capability may legally be named e.g.
	// "health", and its POST operation shares the path with the built-in
	// GET one.
	merge := func(path, method string, op map[string]any) {
		item, _ := paths[path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[path] = item
		}
		item[method] = op
	}
	merge("/v1/", "get", map[string]any{
		"summary": "List capabilities",
		"parameters": []any{map[string]any{
			"name": "all", "in": "query", "required": false,
			"description": "all=1 includes old versions and superseded capabilities",
			"schema":      map[string]string{"type": "string"},
		}},
		"responses": map[string]any{"200": map[string]any{"description": "Capability descriptors"}},
	})
	merge("/v1/health", "get", map[string]any{
		"summary": "Capability readiness",
		"responses": map[string]any{
			"200": map[string]any{"description": "Every listed capability ready"},
			"503": map[string]any{"description": "A capability is not ready, or the job journal has failed"},
		},
	})
	merge("/v1/jobs/{id}", "get", jobOp("Get a job's status (plus result on success)"))
	merge("/v1/jobs/{id}", "delete", jobOp("Cancel a job"))
	merge("/v1/jobs/{id}/events", "get", map[string]any{
		"summary":    "Stream job events as Server-Sent Events",
		"parameters": []any{jobIDParam},
		"responses": map[string]any{
			"200": map[string]any{
				"description": "Job events (data: job Event JSON)",
				"content":     map[string]any{"text/event-stream": map[string]any{}},
			},
			"404": errResp("Job not found (or not visible to this caller)"),
		},
	})
	// Every operation also takes the session header — appended to any
	// parameters the operation already declares, never replacing them.
	for _, ops := range paths {
		for _, op := range ops.(map[string]any) {
			o := op.(map[string]any)
			params, _ := o["parameters"].([]any)
			o["parameters"] = append(params, map[string]string{"$ref": "#/components/parameters/CraggySession"})
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
					"description": "Caller session: sets Caller.Session for this request when the host's caller (auth wrapper or Caller function) did not already set one; it never overrides theirs. Job access defaults to jobs from the caller's own session.",
					"schema":      map[string]string{"type": "string"},
				},
				"JobId": map[string]any{
					"name":        "id",
					"in":          "path",
					"required":    true,
					"description": "Job ID, as returned in a 202 job envelope or by the jobs.* capabilities.",
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
				"JobEnvelope": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"job":    map[string]any{"type": "string", "description": "job ID"},
						"status": map[string]any{"$ref": "#/components/schemas/JobStatus"},
						"result": map[string]any{"description": "the capability's output; present on success where documented"},
					},
					"required": []string{"job", "status"},
				},
				"JobStatus": map[string]any{
					"type":        "object",
					"description": "A registry job status, the same shape the jobs.* capabilities return (id, capability name@version, caller, state, latest stage progress, sequence, timestamps, and on failure an error object with code, optional reason, message and retryable).",
					"properties": map[string]any{
						"id":         map[string]any{"type": "string"},
						"capability": map[string]any{"type": "string"},
						"caller":     map[string]any{"type": "object"},
						"parent":     map[string]any{"type": "string"},
						"state":      map[string]any{"type": "string", "enum": []string{"queued", "running", "stalled", "succeeded", "failed", "cancelled", "lost"}},
						"last": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"stage":   map[string]any{"type": "string"},
								"message": map[string]any{"type": "string"},
								"done":    map[string]any{"type": "integer"},
								"total":   map[string]any{"type": "integer"},
								"unit":    map[string]any{"type": "string"},
								"data":    map[string]any{},
							},
						},
						"seq":          map[string]any{"type": "integer"},
						"supersededBy": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"started":      map[string]any{"type": "string"},
						"heartbeat":    map[string]any{"type": "string"},
						"ended":        map[string]any{"type": "string"},
						"error": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"code":      map[string]any{"type": "string"},
								"reason":    map[string]any{"type": "string"},
								"message":   map[string]any{"type": "string"},
								"retryable": map[string]any{"type": "boolean"},
							},
						},
					},
					"required": []string{"id", "capability", "caller", "state", "last", "seq", "started", "heartbeat"},
				},
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
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]string{"$ref": "#/components/schemas/JobEnvelope"},
				},
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
