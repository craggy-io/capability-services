// Package postgres keeps a capability-services job table in PostgreSQL, so
// every instance of a service shares it (registry.ShareJobs). One row per job
// holds its newest record; the row's seq and state guard it, so an older or
// non-final record never replaces a newer or final one.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/craggy-io/capability-services/registry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures New.
type Options struct {
	// Table is the job table's name (default capability_jobs). Services
	// sharing a database use distinct tables.
	Table string
}

var tableName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Jobs is a registry.SharedJobs over a pool.
type Jobs struct {
	pool  *pgxpool.Pool
	table string
}

var _ registry.SharedJobs = (*Jobs)(nil)

// New returns the job table in pool, creating it if needed. Several processes
// may call it at once.
func New(ctx context.Context, pool *pgxpool.Pool, opts Options) (*Jobs, error) {
	table := opts.Table
	if table == "" {
		table = "capability_jobs"
	}
	if !tableName.MatchString(table) {
		return nil, fmt.Errorf("postgres: invalid job table name %q", table)
	}
	j := &Jobs{pool: pool, table: table}
	if err := j.migrate(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Jobs) migrate(ctx context.Context) error {
	return pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('capability-services/jobs/' || $1))`, j.table); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %[1]s (
			id               text PRIMARY KEY,
			seq              bigint NOT NULL,
			state            text NOT NULL,
			ended            timestamptz,
			cancel_requested boolean NOT NULL DEFAULT false,
			record           json NOT NULL -- json, not jsonb: results keep their exact bytes
		);
		CREATE INDEX IF NOT EXISTS %[1]s_ended ON %[1]s (ended)`, j.table))
		if err != nil {
			return fmt.Errorf("postgres: create job table: %w", err)
		}
		return nil
	})
}

var terminalStates = []string{
	string(registry.JobSucceeded), string(registry.JobFailed), string(registry.JobCancelled), string(registry.JobLost),
}

func endedAt(rec registry.JobRecord) *time.Time {
	if !rec.Status.State.Terminal() || rec.Status.Ended.IsZero() {
		return nil
	}
	t := rec.Status.Ended
	return &t
}

// Put stores rec unless the stored record is final or newer.
func (j *Jobs) Put(ctx context.Context, rec registry.JobRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("postgres: encode job %s: %w", rec.Status.ID, err)
	}
	_, err = j.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (id, seq, state, ended, record) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET seq = excluded.seq, state = excluded.state, ended = excluded.ended, record = excluded.record
		WHERE %[1]s.seq <= excluded.seq AND NOT (%[1]s.state = ANY($6))`, j.table),
		rec.Status.ID, rec.Status.Seq, string(rec.Status.State), endedAt(rec), raw, terminalStates)
	if err != nil {
		return fmt.Errorf("postgres: put job %s: %w", rec.Status.ID, err)
	}
	return nil
}

// Get returns the newest record for id.
func (j *Jobs) Get(ctx context.Context, id string) (registry.JobRecord, bool, error) {
	var raw []byte
	var state string
	var seq int64
	err := j.pool.QueryRow(ctx, fmt.Sprintf(`SELECT record, state, seq FROM %s WHERE id = $1`, j.table), id).Scan(&raw, &state, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return registry.JobRecord{}, false, nil
	}
	if err != nil {
		return registry.JobRecord{}, false, fmt.Errorf("postgres: get job %s: %w", id, err)
	}
	rec, err := decode(raw, state, seq)
	return rec, err == nil, err
}

// decode reads a row. Claim moves state and seq without rewriting the record,
// so the columns win.
func decode(raw []byte, state string, seq int64) (registry.JobRecord, error) {
	var rec registry.JobRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, fmt.Errorf("postgres: decode job: %w", err)
	}
	rec.Status.State = registry.JobState(state)
	rec.Status.Seq = seq
	return rec, nil
}

// List returns unfinished jobs and those ended within keep, deleting older
// ones.
func (j *Jobs) List(ctx context.Context, keep time.Duration) ([]registry.JobRecord, error) {
	if keep > 0 {
		if _, err := j.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE ended < $1`, j.table), time.Now().Add(-keep)); err != nil {
			return nil, fmt.Errorf("postgres: expire jobs: %w", err)
		}
	}
	rows, err := j.pool.Query(ctx, fmt.Sprintf(`SELECT record, state, seq FROM %s ORDER BY id`, j.table))
	if err != nil {
		return nil, fmt.Errorf("postgres: list jobs: %w", err)
	}
	defer rows.Close()
	out := []registry.JobRecord{}
	for rows.Next() {
		var raw []byte
		var state string
		var seq int64
		if err := rows.Scan(&raw, &state, &seq); err != nil {
			return nil, fmt.Errorf("postgres: list jobs: %w", err)
		}
		rec, err := decode(raw, state, seq)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Claim moves a queued job to running; exactly one caller wins.
func (j *Jobs) Claim(ctx context.Context, id string) (bool, error) {
	tag, err := j.pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET state = $2, seq = seq + 1 WHERE id = $1 AND state = $3`, j.table),
		id, string(registry.JobRunning), string(registry.JobQueued))
	if err != nil {
		return false, fmt.Errorf("postgres: claim job %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RequestCancel marks id for cancellation.
func (j *Jobs) RequestCancel(ctx context.Context, id string) error {
	if _, err := j.pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET cancel_requested = true WHERE id = $1`, j.table), id); err != nil {
		return fmt.Errorf("postgres: cancel job %s: %w", id, err)
	}
	return nil
}

// CancelRequested reports whether id was marked for cancellation.
func (j *Jobs) CancelRequested(ctx context.Context, id string) (bool, error) {
	var asked bool
	err := j.pool.QueryRow(ctx, fmt.Sprintf(`SELECT cancel_requested FROM %s WHERE id = $1`, j.table), id).Scan(&asked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: read cancel of job %s: %w", id, err)
	}
	return asked, nil
}
