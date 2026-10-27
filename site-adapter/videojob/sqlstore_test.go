package videojob

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// Run with QSTAR_TEST_PG_DSN pointing at a disposable database that has
// migrations/001_video_jobs.up.sql applied.
func TestSQLStore(t *testing.T) {
	dsn := os.Getenv("QSTAR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("QSTAR_TEST_PG_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM video_jobs`); err != nil {
		t.Fatal(err)
	}
	store := &SQLStore{DB: db}
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))
	job := NewJob("job-sql", "ep01/01/v1", req, []Capability{doubaoCap(), H3Capability("h3-profile", true)}, 5_000_000, time.Now().UTC())
	if _, created, err := store.CreateJob(ctx, job); err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	dup := NewJob("job-other", "ep01/01/v1", req, nil, 0, time.Now().UTC())
	if got, created, err := store.CreateJob(ctx, dup); err != nil || created || got.ID != "job-sql" {
		t.Fatalf("idempotent create returned %+v %v %v", got, created, err)
	}

	p := &fakeProvider{id: "doubao", submitFn: func(int) (string, error) { return "db-task-1", nil },
		pollFn: func(*Attempt) PollResult { return PollResult{State: TaskPending, CostMicroUSD: 250_000} }}
	r := &Runner{Store: store, Providers: map[string]Provider{"doubao": p}}
	for range 2 {
		if _, err := r.Step(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.GetJob(ctx, job.ID)
	if err != nil || got.Current().ExternalTaskID != "db-task-1" || p.submits != 1 {
		t.Fatalf("job %+v submits %d err %v", got, p.submits, err)
	}
	stale := *got
	stale.Revision--
	if err := store.SaveJob(ctx, &stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	var provider, task string
	var cost int64
	if err := db.QueryRowContext(ctx, `SELECT provider, external_task_id, cost_micro_usd FROM video_job_attempts WHERE job_id=$1`, job.ID).Scan(&provider, &task, &cost); err != nil {
		t.Fatal(err)
	}
	if provider != "doubao" || task != "db-task-1" || cost != 250_000 {
		t.Fatalf("attempt view %s %s %d", provider, task, cost)
	}
}
