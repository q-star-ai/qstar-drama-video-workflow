package videojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCLI mimics h3-cloud's output layout and JSON status contract.
const fakeCLI = `#!/bin/sh
if [ "$1" = status ]; then echo '{"status":{"state":"stopped"}}'; exit 0; fi
printf '%s\n' "$@" > args.txt
out=''; while [ "$#" -gt 0 ]; do
  if [ "$1" = '--output-dir' ]; then out="$2"; break; fi
  shift
done
[ -n "$out" ] || exit 2
mkdir -p "$out/job-fake"
printf '{"prompt_id":"p-123"}' > "$out/job-fake/manifest.json"
i=0; while [ ! -f release.txt ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
if [ -f fail.txt ]; then echo boom >&2; exit 3; fi
printf 'mp4' > "$out/job-fake/out.mp4"
printf '{"prompt_id":"p-123","artifact_path":"out.mp4"}' > "$out/job-fake/manifest.json"
`

func h3Fixture(t *testing.T) (*H3CLIProvider, *VideoJob, *Runner, string) {
	t.Helper()
	root := t.TempDir()
	cli := filepath.Join(root, "h3-cloud")
	if err := os.WriteFile(cli, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	img := []byte("approved-frame")
	sum := sha256.Sum256(img)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(img) }))
	t.Cleanup(srv.Close)

	shot := sampleShot(ModeFirstLast)
	for _, r := range []*AssetRef{shot.Previs.FirstFrame, shot.Previs.LastFrame} {
		r.SHA256 = hex.EncodeToString(sum[:])
	}
	req, err := BuildRequest(shot)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	p := &H3CLIProvider{
		Cfg:   H3CLIConfig{Command: cli, ConfigPath: "/worker/h3.yaml", Megapixels: 0.5, WorkRoot: filepath.Join(root, "runs"), HourlyRateMicroUSD: 3_600_000},
		Fetch: &AssetFetcher{Sign: func(uri string) (string, error) { return srv.URL + "/" + strings.TrimPrefix(uri, "asset://"), nil }},
		StoreOutput: func(_ context.Context, j *VideoJob, local string) (string, error) {
			return "asset://videos/" + j.ID + ".mp4", nil
		},
		Now: func() time.Time { return now },
	}
	job := NewJob("job-h3cli", "k", req, []Capability{H3Capability("h3-cli", true)}, 0, now)
	store := NewMemStore()
	store.CreateJob(context.Background(), job)
	r := &Runner{Store: store, Providers: map[string]Provider{"minimax-h3": p}, Now: func() time.Time { return now }}
	return p, job, r, filepath.Join(p.Cfg.WorkRoot, job.ID, "attempt-0")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200 && !cond(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met")
	}
}

func TestH3CLIRunRecordsPromptAndFinishes(t *testing.T) {
	p, job, r, dir := h3Fixture(t)
	ctx := context.Background()
	if _, err := r.Step(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "args.txt")); return err == nil })
	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	got := strings.Fields(string(args))
	for _, want := range []string{"generate", "--first-frame-image", "--last-frame-image", "--megapixels", "--duration-seconds", "--output-dir"} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Fatalf("args %v missing %s", got, want)
		}
	}
	if strings.Contains(string(args), "--reference-image") {
		t.Fatal("key-frame run also sent reference images")
	}
	waitFor(t, func() bool { id, _ := p.manifest(dir); return id != "" })
	r.Step(ctx, job.ID)
	j, _ := r.Store.GetJob(ctx, job.ID)
	if j.Current().ExternalTaskID != "p-123" || j.Status != StatusRunning {
		t.Fatalf("prompt id not recorded: %+v", j.Current())
	}
	os.WriteFile(filepath.Join(dir, "release.txt"), nil, 0o600)
	waitFor(t, func() bool { _, out := p.manifest(dir); return out != "" })
	time.Sleep(100 * time.Millisecond)
	r.Step(ctx, job.ID)
	j, _ = r.Store.GetJob(ctx, job.ID)
	if j.Status != StatusProbing || j.Current().OutputURI != "asset://videos/job-h3cli.mp4" || !j.Current().InstanceStopped {
		t.Fatalf("status %s attempt %+v", j.Status, j.Current())
	}
}

func TestH3CLIRestartWithoutResumeNeedsReconcile(t *testing.T) {
	p, job, r, dir := h3Fixture(t)
	ctx := context.Background()
	r.Step(ctx, job.ID)
	waitFor(t, func() bool { id, _ := p.manifest(dir); return id != "" })
	// New worker process: the CLI run it started is not ours to follow.
	fresh := &H3CLIProvider{Cfg: p.Cfg, Fetch: p.Fetch, StoreOutput: p.StoreOutput, Now: p.Now}
	r2 := &Runner{Store: r.Store, Providers: map[string]Provider{"minimax-h3": fresh}, Now: r.Now}
	r2.Step(ctx, job.ID)
	j, _ := r.Store.GetJob(ctx, job.ID)
	if j.Status != StatusNeedsReconcile || j.Current().ExternalTaskID != "p-123" {
		t.Fatalf("status %s reason %q", j.Status, j.StatusReason)
	}
	// Submitting again must refuse: the run directory exists.
	if _, err := fresh.Submit(ctx, j, j.Current()); err == nil {
		t.Fatal("second submit allowed for the same attempt")
	}
	os.WriteFile(filepath.Join(dir, "release.txt"), nil, 0o600)
}

func TestH3CLIFailureIsNotRetried(t *testing.T) {
	p, job, r, dir := h3Fixture(t)
	ctx := context.Background()
	r.Step(ctx, job.ID)
	waitFor(t, func() bool { id, _ := p.manifest(dir); return id != "" })
	os.WriteFile(filepath.Join(dir, "fail.txt"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "release.txt"), nil, 0o600)
	time.Sleep(300 * time.Millisecond)
	r.Step(ctx, job.ID)
	j, _ := r.Store.GetJob(ctx, job.ID)
	if j.Status != StatusNeedsReconcile || len(j.Attempts) != 1 {
		t.Fatalf("status %s attempts %d", j.Status, len(j.Attempts))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stderr.log")); !strings.Contains(string(b), "boom") {
		t.Fatal("stderr not kept")
	}
}

func TestH3CLIChecksumMismatchBlocksBeforeGPU(t *testing.T) {
	p, job, r, dir := h3Fixture(t)
	ctx := context.Background()
	j, _ := r.Store.GetJob(ctx, job.ID)
	j.Request.References[0].SHA256 = strings.Repeat("0", 64)
	r.Store.SaveJob(ctx, j)
	r.Step(ctx, job.ID)
	j, _ = r.Store.GetJob(ctx, job.ID)
	if j.Status != StatusBlocked || !strings.Contains(j.StatusReason, "checksum") {
		t.Fatalf("status %s %q", j.Status, j.StatusReason)
	}
	if _, err := os.Stat(filepath.Join(dir, "args.txt")); err == nil {
		t.Fatal("CLI ran despite checksum mismatch")
	}
	_ = p
}

func TestH3CLIArgsRejectsMixedModes(t *testing.T) {
	in := H3Input{Mode: ModeFirstLast, Images: []AssetRef{{Purpose: "first_frame"}, {Purpose: "identity:lead"}}, AspectRatio: "9:16", DurationSeconds: 8}
	if _, err := H3CLIArgs(H3CLIConfig{Megapixels: 0.5}, in, "p.txt", []string{"/a", "/b"}); err == nil {
		t.Fatal("mixed key-frame and identity accepted")
	}
	in = H3Input{Mode: ModeIdentityRefs, Images: []AssetRef{{Purpose: "identity:lead"}, {Purpose: "wardrobe:lead"}}, AspectRatio: "9:16", DurationSeconds: 8}
	args, err := H3CLIArgs(H3CLIConfig{Megapixels: 0.5}, in, "p.txt", []string{"/a", "/b"})
	if err != nil || strings.Count(strings.Join(args, " "), "--reference-image") != 2 || strings.Contains(strings.Join(args, " "), "frame-image") {
		t.Fatalf("args %v err %v", args, err)
	}
}

func TestH3CLIOutputWaitsForConfirmedStop(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "h3-cloud")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho '{\"status\":{\"state\":\"running\"}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(output, []byte("mp4"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &H3CLIProvider{Cfg: H3CLIConfig{Command: cli}, StoreOutput: func(context.Context, *VideoJob, string) (string, error) {
		t.Fatal("output must not be published while GPU status is running")
		return "", nil
	}}
	_, err := p.finish(context.Background(), &VideoJob{}, dir, output, PollResult{})
	if !errors.Is(err, ErrResumeUnavailable) {
		t.Fatalf("expected manual reconciliation, got %v", err)
	}
}

// testClock is safe to read from the goroutine that records the CLI exit.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func useClock(p *H3CLIProvider, r *Runner, c *testClock) {
	p.Now, r.Now = c.Now, c.Now
}

func TestH3CLICostCountsOnlyProcessLifetime(t *testing.T) {
	p, job, r, dir := h3Fixture(t) // 3_600_000 micro-USD per hour = 1_000 per second
	clock := &testClock{t: time.Unix(10_000, 0)}
	useClock(p, r, clock)
	ctx := context.Background()
	start := clock.Now()
	r.Step(ctx, job.ID)
	waitFor(t, func() bool { id, _ := p.manifest(dir); return id != "" })

	clock.Set(start.Add(10 * time.Minute))
	r.Step(ctx, job.ID)
	if c := f3Cost(t, r, job.ID); c != 600_000 {
		t.Fatalf("running cost after 10m = %d, want 600000", c)
	}

	// The process exits at +20m; the next poll happens hours later.
	clock.Set(start.Add(20 * time.Minute))
	os.WriteFile(filepath.Join(dir, "release.txt"), nil, 0o600)
	waitFor(t, func() bool { _, out := p.manifest(dir); return out != "" })
	time.Sleep(150 * time.Millisecond)
	clock.Set(start.Add(5 * time.Hour))
	r.Step(ctx, job.ID)
	j, _ := r.Store.GetJob(ctx, job.ID)
	if j.Status != StatusProbing {
		t.Fatalf("status %s (%s)", j.Status, j.StatusReason)
	}
	if c := j.Current(); c.CostMicroUSD != 1_200_000 || c.GPUSeconds != 1200 || !c.CostEstimated {
		t.Fatalf("cost after exit = %d (%vs), want 1200000 (1200s)", c.CostMicroUSD, c.GPUSeconds)
	}
}

func TestH3CLICostDoesNotGrowAfterRestartOrReview(t *testing.T) {
	p, job, r, dir := h3Fixture(t)
	clock := &testClock{t: time.Unix(10_000, 0)}
	useClock(p, r, clock)
	ctx := context.Background()
	start := clock.Now()
	r.Step(ctx, job.ID)
	waitFor(t, func() bool { id, _ := p.manifest(dir); return id != "" })
	clock.Set(start.Add(10 * time.Minute))
	r.Step(ctx, job.ID)

	// Worker restart: the new process does not know the run.
	fresh := &H3CLIProvider{Cfg: p.Cfg, Fetch: p.Fetch, StoreOutput: p.StoreOutput, Now: clock.Now}
	r2 := &Runner{Store: r.Store, Providers: map[string]Provider{"minimax-h3": fresh}, Now: clock.Now}
	clock.Set(start.Add(3 * time.Hour))
	r2.Step(ctx, job.ID)
	j, _ := r2.Store.GetJob(ctx, job.ID)
	if j.Status != StatusNeedsReconcile || j.Current().CostMicroUSD != 600_000 {
		t.Fatalf("after restart status=%s cost=%d, want needs_reconcile 600000", j.Status, j.Current().CostMicroUSD)
	}
	// An operator re-attaches the prompt hours later; polling again must not inflate the cost.
	if err := ResolveReconcile(j, j.Current().ExternalTaskID); err != nil {
		t.Fatal(err)
	}
	if err := r2.Store.SaveJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	clock.Set(start.Add(9 * time.Hour))
	r2.Step(ctx, job.ID)
	if c := f3Cost(t, r2, job.ID); c != 600_000 {
		t.Fatalf("cost after operator re-poll = %d, want 600000", c)
	}
	os.WriteFile(filepath.Join(dir, "release.txt"), nil, 0o600)
}

func f3Cost(t *testing.T, r *Runner, id string) int64 {
	t.Helper()
	j, err := r.Store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j.Current().CostMicroUSD
}
