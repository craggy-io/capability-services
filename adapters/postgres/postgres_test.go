package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/capability-services/adapters/postgres"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/registry/jobstoretest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pool connects to CAPABILITY_SERVICES_POSTGRES_DSN. These tests fail, never
// skip, without it.
func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CAPABILITY_SERVICES_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("set CAPABILITY_SERVICES_POSTGRES_DSN to a PostgreSQL database for these tests")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

var tables atomic.Int64

// fresh gives each test its own table; each handle has its own pool, as
// separate instances would.
func fresh(t *testing.T) func() registry.SharedJobs {
	table := fmt.Sprintf("jobs_test_%d_%d", time.Now().UnixNano(), tables.Add(1))
	cleanup := pool(t)
	t.Cleanup(func() { _, _ = cleanup.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	return func() registry.SharedJobs {
		j, err := postgres.New(context.Background(), pool(t), postgres.Options{Table: table})
		if err != nil {
			t.Error(err)
			return nil
		}
		return j
	}
}

func TestConformance(t *testing.T) {
	jobstoretest.Run(t, fresh)
}

// TestTwoRegistriesShareJobs follows and cancels a job through a second
// registry over a second pool.
func TestTwoRegistriesShareJobs(t *testing.T) {
	open := fresh(t)
	slow := registry.Func("demo.slow", "Slow", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, registry.AsLong(time.Minute))
	start := func() *registry.Registry {
		r := registry.New(registry.ShareJobs(open()))
		if err := r.Add(slow); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		return r
	}
	a, b := start(), start()
	job, err := a.Start(context.Background(), "demo.slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	remote, ok := b.Job(job.ID())
	if !ok {
		t.Fatal("b cannot see a's job")
	}
	remote.Cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := remote.Wait(ctx); registry.CodeOf(err) != registry.Cancelled {
		t.Fatal(err)
	}
}

func TestInvalidTableName(t *testing.T) {
	if _, err := postgres.New(context.Background(), pool(t), postgres.Options{Table: "jobs; drop"}); err == nil {
		t.Fatal("accepted an unsafe table name")
	}
}
