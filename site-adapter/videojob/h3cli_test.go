package videojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCLI mimics `h3-cloud`: generate logs its args, writes the manifest with
// a prompt_id, then the MP4 once release.txt exists; status prints a marker.
const fakeCLI = `#!/bin/sh
if [ "$1" = status ]; then echo "instance: stopped"; exit 0; fi
printf '%s\n' "$@" > args.txt
printf '{"prompt_id":"p-123"}' > manifest.json
i=0; while [ ! -f release.txt ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
if [ -f fail.txt ]; then echo boom >&2; exit 3; fi
printf 'mp4' > out.mp4
printf '{"prompt_id":"p-123","output_path":"out.mp4"}' > manifest.json
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
		Cfg:   H3CLIConfig{Command: cli, ConfigPath: "/worker/h3.yaml", Megapixels: 0.5, WorkRoot: filepath.Join(root, "runs"), StoppedMarker: "stopped", HourlyRateMicroUSD: 3_600_000},
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
	for _, want := range []string{"generate", "--first-frame-image", "--last-frame-image", "--megapixels", "--duration-seconds"} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Fatalf("args %v missing %s", got, want)
		}
	}
	if strings.Contains(string(args), "--reference-image") {
		t.Fatal("key-frame run also sent reference images")
	}
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
