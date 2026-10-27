//go:build acceptance

// Isolated acceptance for the workflow adapter. Everything is synthetic: a
// throwaway Postgres (QSTAR_TEST_PG_DSN), generated media, fake providers, a
// fake h3-cloud CLI, an in-process asset server and a controllable clock. No
// real model, GPU, credential, customer asset or production database is used.
//
//	QSTAR_TEST_PG_DSN=postgres://... go test -tags acceptance -run Acceptance -v ./videojob
//
// Site-only behaviour (shot spec pinning, 409 on stale confirmations,
// billing, refunds, probe retry/admin review, storyboard write-back,
// permissions) lives in qstar-drama-site-recovery and is not covered here.
package videojob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func evidence(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("EVIDENCE: "+format, args...)
}

func acceptanceDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("QSTAR_TEST_PG_DSN")
	if dsn == "" {
		t.Fatal("QSTAR_TEST_PG_DSN must point at a throwaway database with migrations/001 applied")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`DELETE FROM video_jobs`); err != nil {
		t.Fatal(err)
	}
	return db
}

func runFFprobe(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
}

// AC-01: the request carries exactly the approved face (ID, version,
// sha256); a file that no longer matches its approved checksum is refused
// before any provider or GPU is touched.
func TestAcceptanceAC01IdentityBoundByVersionAndChecksum(t *testing.T) {
	served := []byte("approved face v1")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write(served)
	}))
	defer srv.Close()
	sum := sha256.Sum256(served)
	shot := sampleShot(ModeIdentityRefs)
	shot.Cast[0].Identity = AssetRef{AssetID: "face-17", Version: "v1", SHA256: hex.EncodeToString(sum[:]), URI: "asset://approved/17"}
	req, err := BuildRequest(shot)
	if err != nil {
		t.Fatal(err)
	}
	face := req.References[0]
	if face.Purpose != "identity:lead" || face.AssetID != "face-17" || face.Version != "v1" || face.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("identity reference %+v", face)
	}
	evidence(t, "request binds identity:lead -> asset %s %s sha256 %s…", face.AssetID, face.Version, face.SHA256[:12])

	fetch := &AssetFetcher{Sign: func(string) (string, error) { return srv.URL + "/face.png", nil }}
	if err := fetch.Verify(context.Background(), []AssetRef{face}); err != nil {
		t.Fatalf("approved file rejected: %v", err)
	}
	served = []byte("someone replaced the file")
	err = fetch.Verify(context.Background(), []AssetRef{face})
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Class != ClassInvalidInput {
		t.Fatalf("replaced file accepted: %v", err)
	}
	evidence(t, "replaced file refused as %s: %s", pe.Class, pe.Message)
}

// AC-02: the same idempotency key returns the original job (one row); a new
// key, i.e. a deliberate re-take, creates a new job. Runs on Postgres.
func TestAcceptanceAC02IdempotentCreateAndDeliberateRetake(t *testing.T) {
	db := acceptanceDB(t)
	api := &API{Store: &SQLStore{DB: db}, NewID: func() string { return "job-" + time.Now().Format("150405.000000000") },
		Capabilities: func(context.Context) []Capability { return []Capability{doubaoCap(), H3Capability("h3-profile", true)} }}
	mux := http.NewServeMux()
	api.Routes(mux, "/api/video")
	body, _ := json.Marshal(sampleShot(ModeIdentityRefs))
	post := func(key string) (int, VideoJob) {
		r := httptest.NewRequest("POST", "/api/video/jobs", bytes.NewReader(body))
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var j VideoJob
		json.Unmarshal(w.Body.Bytes(), &j)
		return w.Code, j
	}
	c1, j1 := post("ep01/01/r3/identity_refs/uuid-a")
	c2, j2 := post("ep01/01/r3/identity_refs/uuid-a")
	var rows int
	db.QueryRow(`SELECT count(*) FROM video_jobs`).Scan(&rows)
	if c1 != 202 || c2 != 200 || j1.ID != j2.ID || rows != 1 {
		t.Fatalf("replay: codes %d/%d ids %s/%s rows %d", c1, c2, j1.ID, j2.ID, rows)
	}
	evidence(t, "same key twice -> %d then %d, job %s, %d row", c1, c2, j1.ID, rows)
	c3, j3 := post("ep01/01/r3/identity_refs/uuid-b")
	db.QueryRow(`SELECT count(*) FROM video_jobs`).Scan(&rows)
	if c3 != 202 || j3.ID == j1.ID || rows != 2 {
		t.Fatalf("re-take: code %d id %s rows %d", c3, j3.ID, rows)
	}
	evidence(t, "deliberate re-take (new key) -> %d, new job %s, %d rows", c3, j3.ID, rows)
}

// AC-03: H3 cost follows the CLI process only, and the persisted cost does
// not grow after the process exits, after a worker restart, or while a job
// waits for an operator. Runs on Postgres.
func TestAcceptanceAC03H3CostFollowsProcessLifetime(t *testing.T) {
	db := acceptanceDB(t)
	cost := func(id string) (doc int64, column int64) {
		db.QueryRow(`SELECT total_cost_micro_usd FROM video_jobs WHERE id = $1`, id).Scan(&column)
		j, err := (&SQLStore{DB: db}).GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return j.Current().CostMicroUSD, column
	}
	setup := func(name string) (*H3CLIProvider, *VideoJob, *Runner, string, *testClock) {
		// The fake CLI always reports prompt_id p-123 and the adapter's schema
		// keeps (provider, external task) unique, so scenarios do not share rows.
		if _, err := db.Exec(`DELETE FROM video_jobs`); err != nil {
			t.Fatal(err)
		}
		p, job, _, _ := h3Fixture(t) // 3_600_000 micro-USD per hour
		// Each scenario is its own job; the fixture's ID and key are shared.
		job.ID, job.IdempotencyKey = "job-ac03-"+name, "k-ac03-"+name
		dir := filepath.Join(p.Cfg.WorkRoot, job.ID, "attempt-0")
		clock := &testClock{t: time.Unix(20_000, 0)}
		store := &SQLStore{DB: db}
		if _, _, err := store.CreateJob(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		r := &Runner{Store: store, Providers: map[string]Provider{"minimax-h3": p}}
		useClock(p, r, clock)
		return p, job, r, dir, clock
	}
	ctx := context.Background()

	p, job, r, dir, clock := setup("exit")
	start := clock.Now()
	r.Step(ctx, job.ID)
	waitFor(t, func() bool { id, _ := p.manifest(dir); return id != "" })
	clock.Set(start.Add(10 * time.Minute))
	r.Step(ctx, job.ID)
	clock.Set(start.Add(20 * time.Minute))
	os.WriteFile(filepath.Join(dir, "release.txt"), nil, 0o600)
	waitFor(t, func() bool { _, out := p.manifest(dir); return out != "" })
	time.Sleep(150 * time.Millisecond)
	clock.Set(start.Add(5 * time.Hour))
	r.Step(ctx, job.ID)
	doc, col := cost(job.ID)
	if doc != 1_200_000 || col != 1_200_000 {
		t.Fatalf("after exit: doc %d column %d, want 1200000", doc, col)
	}
	evidence(t, "process ran 20m, polled 5h after start -> cost %d micro-USD (doc) / %d (column)", doc, col)

	p, job, r, dir, clock = setup("restart")
	start = clock.Now()
	r.Step(ctx, job.ID)
	waitFor(t, func() bool { id, _ := p.manifest(dir); return id != "" })
	clock.Set(start.Add(10 * time.Minute))
	r.Step(ctx, job.ID)
	fresh := &H3CLIProvider{Cfg: p.Cfg, Fetch: p.Fetch, StoreOutput: p.StoreOutput, Now: clock.Now}
	r2 := &Runner{Store: r.Store, Providers: map[string]Provider{"minimax-h3": fresh}, Now: clock.Now}
	clock.Set(start.Add(3 * time.Hour))
	if _, err := r2.Step(ctx, job.ID); err != nil {
		t.Logf("restart step error: %v", err)
	}
	j, _ := r2.Store.GetJob(ctx, job.ID)
	if j.Status != StatusNeedsReconcile {
		t.Fatalf("restart status %s reason %q attempt %+v", j.Status, j.StatusReason, j.Current())
	}
	if err := ResolveReconcile(j, j.Current().ExternalTaskID); err != nil {
		t.Fatal(err)
	}
	r2.Store.SaveJob(ctx, j)
	clock.Set(start.Add(9 * time.Hour))
	r2.Step(ctx, job.ID)
	doc, col = cost(job.ID)
	if doc != 600_000 || col != 600_000 {
		t.Fatalf("after restart and review: doc %d column %d, want 600000", doc, col)
	}
	evidence(t, "running 10m, worker restart, operator re-attach at 9h -> cost stays %d (column %d)", doc, col)
	os.WriteFile(filepath.Join(dir, "release.txt"), nil, 0o600)
}

// AC-04: ffprobe on real synthetic files. A playable file with the wrong
// shape is a format mismatch (a generated video); a file with no video
// stream is flagged unplayable (technical failure); an unreadable file makes
// ffprobe itself fail, and its error is logged without URL credentials.
func TestAcceptanceAC04ProbeClassification(t *testing.T) {
	dir := t.TempDir()
	gen := func(name string, args ...string) string {
		path := filepath.Join(dir, name)
		cmd := exec.Command("ffmpeg", append(append([]string{"-v", "error"}, args...), path)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %s: %v %s", name, err, out)
		}
		return path
	}
	good := gen("good.mp4", "-f", "lavfi", "-i", "testsrc2=size=720x1280:rate=24:duration=8", "-f", "lavfi", "-i", "sine=duration=8", "-shortest", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac")
	wide := gen("wide.mp4", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=8", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	audio := gen("audio-only.m4a", "-f", "lavfi", "-i", "sine=duration=8", "-c:a", "aac")
	garbage := filepath.Join(dir, "garbage.mp4")
	os.WriteFile(garbage, []byte("<html>gateway error</html>"), 0o600)
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))

	check := func(path string) Output {
		data, err := runFFprobe(t, path)
		if err != nil {
			t.Fatalf("ffprobe %s: %v", filepath.Base(path), err)
		}
		out, err := ParseFFprobe(data, "asset://out/"+filepath.Base(path))
		if err != nil {
			t.Fatal(err)
		}
		return CheckOutput(req, out, false)
	}
	if o := check(good); len(o.ProbeFailures) != 0 || o.Unplayable {
		t.Fatalf("good: %+v", o)
	} else {
		evidence(t, "good.mp4 -> %dx%d %.2fs audio=%v, no failures", o.Width, o.Height, o.DurationSeconds, o.HasAudio)
	}
	if o := check(wide); o.Unplayable || len(o.ProbeFailures) != 1 {
		t.Fatalf("wide: %+v", o)
	} else {
		evidence(t, "wide.mp4 -> playable, failures %v (generated video, not refundable by rule)", o.ProbeFailures)
	}
	if o := check(audio); !o.Unplayable {
		t.Fatalf("audio-only: %+v", o)
	} else {
		evidence(t, "audio-only.m4a -> unplayable=%v, failures %v (technical failure, refundable by rule)", o.Unplayable, o.ProbeFailures)
	}
	_, err := runFFprobe(t, garbage)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("garbage file: %v", err)
	}
	msg := "https://cdn.example.test/out.mp4?e=1700000000&token=SECRET: " + strings.TrimSpace(string(exitErr.Stderr))
	logged := RedactForLog(msg, 500)
	if strings.Contains(logged, "SECRET") || !strings.Contains(logged, "?[redacted]") {
		t.Fatalf("logged %q", logged)
	}
	evidence(t, "garbage.mp4 -> ffprobe exit %d, logged %q", exitErr.ExitCode(), logged)
}

// AC-05: a generated video reaches "succeeded" only after a human QC pass
// with three samples; a QC failure keeps the output (a generated video).
func TestAcceptanceAC05HumanQCGatesSuccess(t *testing.T) {
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))
	now := time.Unix(30_000, 0)
	out := CheckOutput(req, Output{AssetURI: "asset://out/a.mp4", DurationSeconds: 8, Width: 720, Height: 1280, VideoCodec: "h264", HasAudio: true}, false)

	pass := &VideoJob{ID: "qc-pass", Request: req, Status: StatusProbing}
	if err := pass.RecordProbe(out, now); err != nil || pass.Status != StatusQCPending {
		t.Fatalf("probe: %v %s", err, pass.Status)
	}
	if err := pass.RecordQC(QCResult{Passed: true, Reviewer: "user:3", CheckedAt: now, SampledAt: []float64{0.5}}); err == nil {
		t.Fatal("pass with one sample accepted")
	}
	if err := pass.RecordQC(QCResult{Passed: true, Reviewer: "user:3", CheckedAt: now, SampledAt: []float64{0.5, 4, 7.5}}); err != nil || pass.Status != StatusSucceeded {
		t.Fatalf("qc pass: %v %s", err, pass.Status)
	}
	evidence(t, "qc_pending -> one-sample pass rejected -> three-sample pass -> %s", pass.Status)

	fail := &VideoJob{ID: "qc-fail", Request: req, Status: StatusProbing}
	fail.RecordProbe(out, now)
	if err := fail.RecordQC(QCResult{Passed: false, Failures: []string{QCFaceDrift, QCCameraDrift}, Reviewer: "user:3", CheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	if fail.Status != StatusFailed || fail.Output == nil || fail.Output.Unplayable {
		t.Fatalf("qc fail: %+v", fail)
	}
	evidence(t, "qc failure [face_drift camera_drift] -> %s with playable output kept (not refundable by rule)", fail.Status)
}

// AC-06: an uncertain submit waits for an operator; attaching the provider
// task resumes polling without a new paid submit, and only an explicit
// resubmit starts a new attempt. Review fields are not touched by either.
func TestAcceptanceAC06OperatorReconcile(t *testing.T) {
	var submits atomic.Int32
	p := &fakeProvider{id: "doubao", submitFn: func(n int) (string, error) {
		submits.Add(1)
		if n == 1 {
			return "", errors.New("Post https://ark: context deadline exceeded")
		}
		return "db-retake", nil
	}, pollFn: func(*Attempt) PollResult { return PollResult{State: TaskPending} }}
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))
	ctx := context.Background()
	for _, tc := range []struct{ name, attach string }{{"attach", "db-found"}, {"resubmit", ""}} {
		submits.Store(0)
		p.submits = 0
		job := NewJob("job-"+tc.name, "k-"+tc.name, req, []Capability{doubaoCap()}, 0, time.Unix(0, 0))
		store := NewMemStore()
		store.CreateJob(ctx, job)
		r := &Runner{Store: store, Providers: map[string]Provider{"doubao": p}}
		r.Step(ctx, job.ID)
		r.Step(ctx, job.ID)
		j, _ := store.GetJob(ctx, job.ID)
		if j.Status != StatusNeedsReconcile || submits.Load() != 1 {
			t.Fatalf("%s: status %s submits %d", tc.name, j.Status, submits.Load())
		}
		if err := ResolveReconcile(j, tc.attach); err != nil {
			t.Fatal(err)
		}
		store.SaveJob(ctx, j)
		r.Step(ctx, job.ID)
		j, _ = store.GetJob(ctx, job.ID)
		wantSubmits, wantTask := int32(1), tc.attach
		if tc.attach == "" {
			wantSubmits, wantTask = 2, "db-retake"
		}
		if submits.Load() != wantSubmits || j.Current().ExternalTaskID != wantTask || j.ReconcileReviewedBy != "" {
			t.Fatalf("%s: submits %d task %q review %q", tc.name, submits.Load(), j.Current().ExternalTaskID, j.ReconcileReviewedBy)
		}
		evidence(t, "%s: needs_reconcile after 1 uncertain submit -> paid submits %d, task %s, attempts %d", tc.name, submits.Load(), j.Current().ExternalTaskID, len(j.Attempts))
	}
}
