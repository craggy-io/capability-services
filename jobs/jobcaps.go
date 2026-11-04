// Package jobs provides the jobs.* capabilities (list, get, wait, cancel)
// over the registry's job table, and their group. A host adds them like any
// other capabilities and every projection gets them.
package jobs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/capability-services/registry"
)

// jobStatusSchema is hand-written because JobStatus contains the recursive
// Caller type (Parent), whose schema cannot be derived by reflection.
func jobStatusSchema() *jsonschema.Schema {
	s, err := registry.ParseSchema(json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"},
    "capability": {"type": "string", "description": "exact name@version"},
    "caller": {"type": "object"},
    "parent": {"type": "string", "description": "job whose handler started this one; cancelling it cancels this job"},
    "state": {"type": "string", "enum": ["queued","running","stalled","succeeded","failed","cancelled","lost"]},
    "last": {"type": "object", "description": "latest stage progress (stage, message, done, total, unit, data); never whole-job progress",
      "properties": {
        "stage": {"type": "string"}, "message": {"type": "string"},
        "done": {"type": "integer"}, "total": {"type": "integer"}, "unit": {"type": "string"}
      }},
    "seq": {"type": "integer"},
    "supersededBy": {"type": "array", "items": {"type": "string"}},
    "started": {"type": "string"}, "heartbeat": {"type": "string"}, "ended": {"type": "string"},
    "error": {"type": "object", "properties": {"code": {"type": "string"}, "reason": {"type": "string", "description": "optional stable lower_snake_case refinement of code, e.g. no_capacity"}, "message": {"type": "string"}}}
  },
  "required": ["id", "capability", "caller", "state", "last", "seq", "started", "heartbeat"]
}`))
	if err != nil {
		panic("jobs: job status schema: " + err.Error())
	}
	return s
}

func jobStatusPageSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"items": {Type: "array", Items: jobStatusSchema()},
			"next":  {Type: "string"},
		},
		Required: []string{"items"},
	}
}

func jobWaitSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"status": jobStatusSchema(),
			"result": {},
		},
		Required: []string{"status"},
	}
}

type jobsListIn struct {
	State      string `json:"state,omitempty" jsonschema:"only jobs in this state (queued, running, stalled, succeeded, failed, cancelled, lost)"`
	Capability string `json:"capability,omitempty" jsonschema:"capability name or glob, e.g. apk.*"`
	registry.PageIn
}

type jobIDIn struct {
	ID string `json:"id" jsonschema:"job id"`
}

type jobsWaitIn struct {
	ID      string `json:"id" jsonschema:"job id"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"seconds to wait, at most 60; 0 returns immediately with the current status"`
}

// WaitOut is returned by jobs.wait (and mirrors what projections show):
// the status, plus the result when the job succeeded.
type WaitOut struct {
	Status registry.JobStatus `json:"status"`
	Result json.RawMessage    `json:"result,omitempty"`
}

func viewOrErr(ctx context.Context) (*registry.View, error) {
	v := registry.ViewFrom(ctx)
	if v == nil {
		return nil, registry.Errorf(registry.Unavailable, "job capabilities must be called through a registry view")
	}
	return v, nil
}

func jobForCaller(ctx context.Context, id string) (*registry.Job, error) {
	v, err := viewOrErr(ctx)
	if err != nil {
		return nil, err
	}
	j, ok := v.JobFor(ctx, id)
	if !ok {
		return nil, registry.Errorf(registry.NotFound, "job %q not found", id)
	}
	return j, nil
}

// Group is the group of Capabilities, for AddGroup. It loads
// always: an agent that starts a long capability needs the job
// controls to follow its handle.
func Group() registry.Group {
	return registry.Group{Name: "jobs", Description: "Follow, wait for and cancel long-running capability jobs.", LoadAlways: true}
}

// Capabilities returns jobs.list, jobs.get, jobs.wait and jobs.cancel.
// The application adds them like any other capabilities and every
// projection gets them. Each acts only on jobs whose capability is in the
// caller's view and which the job-access rule allows.
//
// jobs.wait blocks for up to timeout seconds (60 at most) and returns the
// status plus the result if the job finished. A failed job is reported in
// the status, not as a call error, so agents can poll without a busy loop.
func Capabilities() []registry.Capability {
	list := registry.Func("jobs.list", "List jobs visible to the caller",
		func(ctx context.Context, in jobsListIn) (registry.Page[registry.JobStatus], error) {
			v, err := viewOrErr(ctx)
			if err != nil {
				return registry.Page[registry.JobStatus]{}, err
			}
			f := registry.JobFilter{State: registry.JobState(in.State), Capability: in.Capability}
			if f.State != "" && !validJobState(f.State) {
				return registry.Page[registry.JobStatus]{}, registry.Errorf(registry.Invalid, "unknown job state %q", in.State)
			}
			return registry.Paginate(v.JobsFor(ctx, f), in.PageIn)
		},
		registry.Describe("List long-running jobs the caller may see, oldest first."),
		registry.WithOutput(jobStatusPageSchema()),
		registry.WithEffects(registry.ReadOnly|registry.Idempotent), registry.WithTags("jobs"))

	get := registry.Func("jobs.get", "Get one job's status",
		func(ctx context.Context, in jobIDIn) (registry.JobStatus, error) {
			j, err := jobForCaller(ctx, in.ID)
			if err != nil {
				return registry.JobStatus{}, err
			}
			return j.Status(), nil
		},
		registry.Describe("Return the status, latest stage progress and heartbeat of one job."),
		registry.WithOutput(jobStatusSchema()),
		registry.WithEffects(registry.ReadOnly|registry.Idempotent), registry.WithTags("jobs"))

	wait := registry.Func("jobs.wait", "Wait for a job to finish",
		func(ctx context.Context, in jobsWaitIn) (WaitOut, error) {
			j, err := jobForCaller(ctx, in.ID)
			if err != nil {
				return WaitOut{}, err
			}
			timeout := in.Timeout
			if timeout < 0 {
				return WaitOut{}, registry.Errorf(registry.Invalid, "timeout must be >= 0")
			}
			if timeout > 60 {
				timeout = 60
			}
			if timeout > 0 {
				wctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
				_, _ = j.Wait(wctx) // timeout just means "still running"
				cancel()
				if ctx.Err() != nil {
					return WaitOut{}, registry.WrapCode(registry.Cancelled, ctx.Err())
				}
			}
			out := WaitOut{Status: j.Status()}
			if res, ok := j.Result(); ok {
				out.Result = res
			}
			return out, nil
		},
		registry.Describe("Block up to timeout seconds (max 60) for a job; returns its status and, on success, its result. Poll with this instead of a busy loop."),
		registry.WithOutput(jobWaitSchema()),
		registry.WithEffects(registry.ReadOnly|registry.Idempotent), registry.WithTags("jobs"))

	cancel := registry.Func("jobs.cancel", "Cancel a job",
		func(ctx context.Context, in jobIDIn) (registry.JobStatus, error) {
			j, err := jobForCaller(ctx, in.ID)
			if err != nil {
				return registry.JobStatus{}, err
			}
			j.Cancel()
			return j.Status(), nil
		},
		registry.Describe("Request cancellation of a job. The returned status may still be running until the handler notices; poll jobs.wait."),
		registry.WithOutput(jobStatusSchema()),
		registry.WithEffects(registry.Idempotent), registry.WithTags("jobs"))

	return []registry.Capability{list, get, wait, cancel}
}

func validJobState(s registry.JobState) bool {
	switch s {
	case registry.JobQueued, registry.JobRunning, registry.JobStalled, registry.JobSucceeded, registry.JobFailed, registry.JobCancelled, registry.JobLost:
		return true
	}
	return false
}
