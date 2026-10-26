package videojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ref(id, uri string) AssetRef {
	return AssetRef{AssetID: id, Version: "v1", SHA256: strings.Repeat("a", 64), URI: uri}
}

func sampleShot(mode InputMode) Shot {
	first, last, grey := ref("shot-01-first", "asset://previs/shot-01-first"), ref("shot-01-last", "asset://previs/shot-01-last"), ref("shot-01-greybox", "asset://previs/shot-01-greybox")
	return Shot{
		EpisodeID: "ep01", ShotID: "01", DurationSeconds: 8, AspectRatio: "9:16",
		Cast:        []Character{{ID: "lead", Identity: ref("lead-face", "asset://approved/lead-face")}},
		Wardrobe:    map[string]AssetRef{"lead": ref("lead-office", "asset://approved/lead-office")},
		Environment: ref("office-set", "asset://approved/office-set"),
		InputMode:   mode,
		VisualEN:    "An adult woman in a quiet office at dusk.", ActionEN: "She looks up slowly.", CameraEN: "Eye-level medium close-up, slow dolly in.",
		DialogueZH: "这里怎么一个人都没有？",
		Previs:     Previs{FirstFrame: &first, LastFrame: &last, Greybox: &grey, Camera: map[string]any{"focal_mm": 35}, ReviewedBy: "director"},
	}
}

func doubaoCap() Capability {
	return Capability{Provider: "doubao", Model: "site-configured", Enabled: true, Verification: "verified",
		Modes:      map[InputMode]ModeLimits{ModeIdentityRefs: {MinImages: 1, MaxImages: 4, AcceptsIdentityImages: true}, ModeFirstLast: {MinImages: 1, MaxImages: 2}},
		MinSeconds: 5, MaxSeconds: 10, AspectRatios: []string{"9:16", "16:9"}}
}

func purposes(refs []AssetRef) string {
	var p []string
	for _, r := range refs {
		p = append(p, r.Purpose)
	}
	return strings.Join(p, ",")
}

func TestBuildRequestBindsAssetsPerMode(t *testing.T) {
	req, err := BuildRequest(sampleShot(ModeIdentityRefs))
	if err != nil {
		t.Fatal(err)
	}
	if got := purposes(req.References); got != "identity:lead,wardrobe:lead,environment" {
		t.Fatalf("identity_refs bound %s", got)
	}
	if req.IdentityRisk != "" {
		t.Fatal("identity_refs must not carry identity risk")
	}
	req, err = BuildRequest(sampleShot(ModeFirstLast))
	if err != nil {
		t.Fatal(err)
	}
	if got := purposes(req.References); got != "first_frame,last_frame" || req.IdentityRisk == "" {
		t.Fatalf("first_last bound %s risk=%q", got, req.IdentityRisk)
	}
	req, err = BuildRequest(sampleShot(ModeVideoRef))
	if err != nil {
		t.Fatal(err)
	}
	if got := purposes(req.References); got != "identity:lead,wardrobe:lead,environment,greybox" {
		t.Fatalf("video_ref bound %s", got)
	}
}

func TestBuildRequestRejects(t *testing.T) {
	cases := map[string]func(*Shot){
		"duration":       func(s *Shot) { s.DurationSeconds = 12 },
		"wardrobe":       func(s *Shot) { s.Wardrobe = map[string]AssetRef{} },
		"checksum":       func(s *Shot) { s.Environment.SHA256 = "" },
		"signed url":     func(s *Shot) { s.Environment.URI = "https://cdn.example/office.png?token=abc" },
		"public url":     func(s *Shot) { s.Environment.URI = "https://cdn.example/office.png" },
		"unreviewed":     func(s *Shot) { s.InputMode = ModeFirstLast; s.Previs.ReviewedBy = "" },
		"no first":       func(s *Shot) { s.InputMode = ModeFirstLast; s.Previs.FirstFrame = nil },
		"no greybox":     func(s *Shot) { s.InputMode = ModeVideoRef; s.Previs.Greybox = nil },
		"unknown mode":   func(s *Shot) { s.InputMode = "greybox_only" },
		"duplicate cast": func(s *Shot) { s.Cast = append(s.Cast, s.Cast[0]) },
	}
	for name, mutate := range cases {
		s := sampleShot(ModeIdentityRefs)
		mutate(&s)
		if _, err := BuildRequest(s); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestCandidateChainRespectsH3Limits(t *testing.T) {
	caps := []Capability{H3Capability("h3-profile", true), doubaoCap()}
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))
	chain, _ := CandidateChain(caps, req)
	if len(chain) != 2 || chain[0].Provider != "doubao" || chain[1].Provider != "minimax-h3" {
		t.Fatalf("want verified primary then H3 fallback, got %+v", chain)
	}
	req, _ = BuildRequest(sampleShot(ModeVideoRef))
	chain, rejected := CandidateChain(caps, req)
	if len(chain) != 0 || !strings.Contains(rejected["minimax-h3"], "not supported") {
		t.Fatalf("greybox must not route to H3: chain=%v rejected=%v", chain, rejected)
	}
	// A first_last request smuggling identity images is refused by H3.
	req, _ = BuildRequest(sampleShot(ModeFirstLast))
	req.References = append(req.References, AssetRef{AssetID: "lead-face", Purpose: "identity:lead"})
	if ok, why := H3Capability("p", true).Supports(req); ok || !strings.Contains(why, "identity") {
		t.Fatalf("H3 accepted identity images with first_last: %s", why)
	}
	if _, err := H3InputFor(req, "x"); err == nil {
		t.Fatal("H3InputFor accepted mixed modes")
	}
}

// fakeProvider scripts submit/poll outcomes and counts paid submits.
type fakeProvider struct {
	id       string
	submits  int
	submitFn func(n int) (string, error)
	pollFn   func(att *Attempt) PollResult
}

func (f *fakeProvider) ID() string { return f.id }
func (f *fakeProvider) Submit(_ context.Context, _ *VideoJob, _ *Attempt) (string, error) {
	f.submits++
	return f.submitFn(f.submits)
}
func (f *fakeProvider) Poll(_ context.Context, _ *VideoJob, att *Attempt) (PollResult, error) {
	return f.pollFn(att), nil
}

func newRun(t *testing.T, mode InputMode, providers ...*fakeProvider) (*Runner, *VideoJob) {
	t.Helper()
	req, err := BuildRequest(sampleShot(mode))
	if err != nil {
		t.Fatal(err)
	}
	h3 := H3Capability("h3-profile", true)
	job := NewJob("job-1", "ep01/01/v1", req, []Capability{h3, doubaoCap()}, 0, time.Unix(0, 0))
	store := NewMemStore()
	if _, _, err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: store, Providers: map[string]Provider{}}
	for _, p := range providers {
		r.Providers[p.id] = p
	}
	return r, job
}

func TestFailoverRecordsSwitchAndResumesWithoutResubmit(t *testing.T) {
	doubao := &fakeProvider{id: "doubao", submitFn: func(int) (string, error) { return "db-1", nil },
		pollFn: func(*Attempt) PollResult {
			return PollResult{State: TaskFailed, Err: &ProviderError{ClassTaskFailed, "async task failed"}}
		}}
	h3 := &fakeProvider{id: "minimax-h3", submitFn: func(int) (string, error) { return "prompt-abc", nil },
		pollFn: func(*Attempt) PollResult { return PollResult{State: TaskPending, CostMicroUSD: 120000} }}
	r, job := newRun(t, ModeIdentityRefs, doubao, h3)
	ctx := context.Background()
	for range 2 {
		if _, err := r.Step(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Simulated worker restart: a fresh Runner over the same store keeps polling prompt-abc.
	r2 := &Runner{Store: r.Store, Providers: r.Providers}
	for range 3 {
		if _, err := r2.Step(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := r.Store.GetJob(ctx, job.ID)
	if h3.submits != 1 || doubao.submits != 1 {
		t.Fatalf("paid submits doubao=%d h3=%d, want 1 each", doubao.submits, h3.submits)
	}
	if len(got.Fallbacks) != 1 || got.Fallbacks[0].From != "doubao" || got.Fallbacks[0].To != "minimax-h3" {
		t.Fatalf("fallbacks %+v", got.Fallbacks)
	}
	if cur := got.Current(); cur.ExternalTaskID != "prompt-abc" || cur.Model != "h3-profile" || cur.CostMicroUSD != 120000 {
		t.Fatalf("current attempt %+v", cur)
	}
}

func TestCrashDuringSubmitNeedsReconcile(t *testing.T) {
	doubao := &fakeProvider{id: "doubao", submitFn: func(int) (string, error) { return "", errors.New("connection reset") },
		pollFn: func(*Attempt) PollResult { return PollResult{State: TaskPending} }}
	r, job := newRun(t, ModeIdentityRefs, doubao)
	ctx := context.Background()
	got, _ := r.Step(ctx, job.ID)
	got, _ = r.Store.GetJob(ctx, job.ID)
	if got.Status != StatusNeedsReconcile {
		t.Fatalf("status %s", got.Status)
	}
	if _, err := r.Step(ctx, job.ID); err != nil || doubao.submits != 1 {
		t.Fatalf("resubmitted while reconcile pending: submits=%d err=%v", doubao.submits, err)
	}
	if err := ResolveReconcile(got, "db-found"); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.SaveJob(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Step(ctx, job.ID); err != nil || doubao.submits != 1 {
		t.Fatalf("after reconcile submits=%d err=%v", doubao.submits, err)
	}
}

func TestInvalidInputBlocksAndSafetyRetryIsSeparate(t *testing.T) {
	doubao := &fakeProvider{id: "doubao", submitFn: func(n int) (string, error) {
		if n == 1 {
			return "", &ProviderError{ClassContentSafety, "prompt flagged"}
		}
		return "db-2", nil
	}, pollFn: func(*Attempt) PollResult { return PollResult{State: TaskSucceeded, OutputURI: "asset://out/01.mp4"} }}
	r, job := newRun(t, ModeIdentityRefs, doubao)
	r.SafetyRewrite = func(p string) string { return "Calm, non-violent scene. " + p }
	ctx := context.Background()
	r.Step(ctx, job.ID)
	r.Step(ctx, job.ID)
	got, _ := r.Store.GetJob(ctx, job.ID)
	if len(got.Attempts) != 2 || got.Attempts[1].Kind != "safety_retry" || len(got.Fallbacks) != 0 || got.Status != StatusProbing {
		t.Fatalf("attempts %+v fallbacks %+v status %s", got.Attempts, got.Fallbacks, got.Status)
	}

	bad := &fakeProvider{id: "doubao", submitFn: func(int) (string, error) { return "", &ProviderError{ClassInvalidInput, "bad ref"} }}
	r, job = newRun(t, ModeIdentityRefs, bad)
	r.Step(ctx, job.ID)
	got, _ = r.Store.GetJob(ctx, job.ID)
	if got.Status != StatusBlocked || len(got.Fallbacks) != 0 {
		t.Fatalf("invalid input should block without switching: %s %+v", got.Status, got.Fallbacks)
	}
}

func TestProbeAndQCGateSuccess(t *testing.T) {
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))
	job := &VideoJob{ID: "j", Request: req, Status: StatusProbing}
	out := CheckOutput(req, Output{AssetURI: "asset://out", DurationSeconds: 8.04, Width: 720, Height: 1280, VideoCodec: "h264"}, false)
	if err := job.RecordProbe(out, time.Now()); err != nil || job.Status != StatusQCPending {
		t.Fatalf("probe: %v %s", err, job.Status)
	}
	if err := job.RecordQC(QCResult{Passed: true, Reviewer: "qa", SampledAt: []float64{0.5}}); err == nil {
		t.Fatal("pass with one sample accepted")
	}
	if err := job.RecordQC(QCResult{Passed: false, Failures: []string{QCFaceDrift}, Reviewer: "qa"}); err != nil || job.Status != StatusFailed {
		t.Fatalf("qc fail: %v %s", err, job.Status)
	}
	bad := CheckOutput(req, Output{DurationSeconds: 6, Width: 1280, Height: 720, VideoCodec: "h264"}, true)
	if len(bad.ProbeFailures) != 3 {
		t.Fatalf("want duration, aspect and audio failures, got %v", bad.ProbeFailures)
	}
}

func TestFFprobeOnRealFile(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "shot.mp4")
	gen := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=720x1280:rate=24:duration=8", "-f", "lavfi", "-i", "sine=duration=8", "-shortest", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot encode here: %v %s", err, out)
	}
	data, err := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseFFprobe(data, "asset://out/shot.mp4")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))
	if out = CheckOutput(req, out, true); len(out.ProbeFailures) != 0 || out.FPS != 24 || !out.HasAudio {
		t.Fatalf("probe %+v", out)
	}
	_ = os.Remove(path)
}

// fakeH3 is an in-memory H3 runtime.
type fakeH3 struct {
	owner           string
	submitted       []H3Input
	state           TaskState
	active, stopped int
}

func (f *fakeH3) AcquireInstance(_ context.Context, jobID string) (H3Lease, error) {
	if f.owner == "" {
		f.owner = jobID
	}
	return H3Lease{InstanceID: "gpu-1", OwnerJobID: f.owner}, nil
}
func (f *fakeH3) Lease(context.Context, string) (H3Lease, error) {
	return H3Lease{InstanceID: "gpu-1", OwnerJobID: f.owner}, nil
}
func (f *fakeH3) Submit(_ context.Context, _ string, in H3Input) (string, error) {
	f.submitted = append(f.submitted, in)
	return "prompt-1", nil
}
func (f *fakeH3) Status(context.Context, string, string) (H3PromptStatus, error) {
	return H3PromptStatus{State: f.state}, nil
}
func (f *fakeH3) StoreOutput(context.Context, string, string, *VideoJob) (string, error) {
	return "asset://out/h3.mp4", nil
}
func (f *fakeH3) ActivePrompts(context.Context, string) (int, error) { return f.active, nil }
func (f *fakeH3) Stop(context.Context, string) error                 { f.stopped++; return nil }

func TestH3ProviderResumesAndStopsOnlyOwnedIdleInstance(t *testing.T) {
	now := time.Unix(1000, 0)
	rt := &fakeH3{state: TaskPending}
	h3 := &H3Provider{Runtime: rt, HourlyRateMicroUSD: 3_600_000, MaxRuntime: time.Hour, Now: func() time.Time { return now }}
	req, _ := BuildRequest(sampleShot(ModeFirstLast))
	job := NewJob("job-h3", "k", req, []Capability{H3Capability("h3-profile", true)}, 0, now)
	store := NewMemStore()
	store.CreateJob(context.Background(), job)
	r := &Runner{Store: store, Providers: map[string]Provider{"minimax-h3": h3}, Now: func() time.Time { return now }}
	ctx := context.Background()
	r.Step(ctx, job.ID)
	now = now.Add(10 * time.Minute)
	r.Step(ctx, job.ID)
	rt.state = TaskSucceeded
	rt.active = 1 // another job's prompt is still on the instance
	r.Step(ctx, job.ID)
	got, _ := store.GetJob(ctx, job.ID)
	att := got.Current()
	if len(rt.submitted) != 1 || rt.submitted[0].Mode != ModeFirstLast || purposes(rt.submitted[0].Images) != "first_frame,last_frame" {
		t.Fatalf("H3 submits %+v", rt.submitted)
	}
	if got.Status != StatusProbing || att.OutputURI != "asset://out/h3.mp4" || att.CostMicroUSD != 600_000 {
		t.Fatalf("status %s attempt %+v", got.Status, att)
	}
	if rt.stopped != 0 || att.InstanceStopped {
		t.Fatal("stopped an instance that still has active prompts")
	}
	rt.active, rt.owner = 0, "someone-else"
	if h3.stopIfOwner(ctx, got, att) || rt.stopped != 0 {
		t.Fatal("stopped an instance this job does not own")
	}
	rt.owner = job.ID
	if !h3.stopIfOwner(ctx, got, att) || rt.stopped != 1 {
		t.Fatal("did not stop owned idle instance")
	}
}

func TestAPIIdempotentCreate(t *testing.T) {
	store := NewMemStore()
	n := 0
	api := &API{Store: store, NewID: func() string { n++; return "job-" + string(rune('0'+n)) },
		Capabilities: func(context.Context) []Capability { return []Capability{doubaoCap(), H3Capability("h3-profile", true)} }}
	mux := http.NewServeMux()
	api.Routes(mux, "/api/video")
	body, _ := json.Marshal(sampleShot(ModeIdentityRefs))
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/video/jobs", bytes.NewReader(body))
		req.Header.Set("Idempotency-Key", "ep01/01/v1")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	first, second := post(), post()
	if first.Code != http.StatusAccepted || second.Code != http.StatusOK {
		t.Fatalf("codes %d %d: %s", first.Code, second.Code, second.Body)
	}
	var a, b VideoJob
	json.Unmarshal(first.Body.Bytes(), &a)
	json.Unmarshal(second.Body.Bytes(), &b)
	if a.ID != b.ID || len(a.Chain) != 2 {
		t.Fatalf("ids %s %s chain %d", a.ID, b.ID, len(a.Chain))
	}

	grey, _ := json.Marshal(sampleShot(ModeVideoRef))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/video/plan", bytes.NewReader(grey)))
	var plan PlanResponse
	json.Unmarshal(w.Body.Bytes(), &plan)
	if len(plan.Chain) != 0 || plan.Rejected["minimax-h3"] == "" {
		t.Fatalf("greybox plan %s", w.Body)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/video/capabilities", nil))
	if strings.Contains(strings.ToLower(w.Body.String()), "key") {
		t.Fatalf("capabilities leak something key-like: %s", w.Body)
	}
}

// resumeLost reports measured cost together with ErrResumeUnavailable, as the
// H3 CLI provider does when the GPU is not confirmed stopped.
type resumeLost struct{ fakeProvider }

func (r *resumeLost) Poll(context.Context, *VideoJob, *Attempt) (PollResult, error) {
	return PollResult{CostMicroUSD: 750_000, CostEstimated: true, GPUSeconds: 900, InstanceStopped: false}, ErrResumeUnavailable
}

func TestCostIsRecordedWhenHandedToOperator(t *testing.T) {
	p := &resumeLost{fakeProvider{id: "doubao", submitFn: func(int) (string, error) { return "task-1", nil }}}
	req, _ := BuildRequest(sampleShot(ModeIdentityRefs))
	job := NewJob("job-cost", "k-cost", req, []Capability{doubaoCap()}, 0, time.Unix(0, 0))
	store := NewMemStore()
	store.CreateJob(context.Background(), job)
	r := &Runner{Store: store, Providers: map[string]Provider{"doubao": p}}
	ctx := context.Background()
	r.Step(ctx, job.ID)
	r.Step(ctx, job.ID)
	got, _ := store.GetJob(ctx, job.ID)
	if got.Status != StatusNeedsReconcile {
		t.Fatalf("status %s", got.Status)
	}
	if cur := got.Current(); cur.CostMicroUSD != 750_000 || !cur.CostEstimated || cur.GPUSeconds != 900 || got.TotalCostMicroUSD() != 750_000 {
		t.Fatalf("cost not recorded before reconcile: %+v", cur)
	}
}
