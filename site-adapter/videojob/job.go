package videojob

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Status is the job state machine. Only succeeded means a probed output that
// passed QC.
type Status string

const (
	StatusQueued         Status = "queued"
	StatusRunning        Status = "running"
	StatusNeedsReconcile Status = "needs_reconcile"
	StatusProbing        Status = "probing"
	StatusQCPending      Status = "qc_pending"
	StatusSucceeded      Status = "succeeded"
	StatusFailed         Status = "failed"
	StatusBlocked        Status = "blocked"
	StatusCancelled      Status = "cancelled"
)

// Terminal reports whether no worker should touch the job again.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusBlocked || s == StatusCancelled
}

// AttemptStatus tracks one submission to one provider.
type AttemptStatus string

const (
	// AttemptSubmitting is persisted before the provider call, so a crash
	// mid-submit is detected instead of silently paying twice.
	AttemptSubmitting AttemptStatus = "submitting"
	AttemptRunning    AttemptStatus = "running"
	AttemptSucceeded  AttemptStatus = "succeeded"
	AttemptFailed     AttemptStatus = "failed"
)

// Attempt records what was actually run and billed.
type Attempt struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Kind is "submit" or "safety_retry"; safety-prompt retries are kept
	// separate from provider switches.
	Kind           string        `json:"kind"`
	PromptEN       string        `json:"prompt_en"`
	ExternalTaskID string        `json:"external_task_id,omitempty"`
	Status         AttemptStatus `json:"status"`
	ErrorClass     ErrorClass    `json:"error_class,omitempty"`
	Error          string        `json:"error,omitempty"`
	OutputURI      string        `json:"output_uri,omitempty"`
	CostMicroUSD   int64         `json:"cost_micro_usd"`
	// CostEstimated is true when CostMicroUSD is rate-based rather than a
	// provider invoice, including an H3 attempt-time estimate.
	CostEstimated bool    `json:"cost_estimated,omitempty"`
	GPUSeconds    float64 `json:"gpu_seconds,omitempty"`
	// InstanceID and InstanceStopped are set by GPU providers such as H3.
	InstanceID      string    `json:"instance_id,omitempty"`
	InstanceStopped bool      `json:"instance_stopped,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
}

// FallbackEvent records each provider switch and why.
type FallbackEvent struct {
	From   string     `json:"from"`
	To     string     `json:"to"`
	Class  ErrorClass `json:"class"`
	Reason string     `json:"reason"`
	At     time.Time  `json:"at"`
}

// Output is what ffprobe measured on the stored file.
type Output struct {
	AssetURI        string   `json:"asset_uri"`
	DurationSeconds float64  `json:"duration_seconds"`
	Width           int      `json:"width"`
	Height          int      `json:"height"`
	FPS             float64  `json:"fps"`
	VideoCodec      string   `json:"video_codec"`
	HasAudio        bool     `json:"has_audio"`
	SizeBytes       int64    `json:"size_bytes"`
	ProbeFailures   []string `json:"probe_failures,omitempty"`
	Unplayable      bool     `json:"unplayable,omitempty"` // confirmed no readable video, distinct from format mismatch
}

// ProbeFailure is one failed automatic probe or output transfer.
type ProbeFailure struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	ExitCode int       `json:"exit_code"` // -1 when the tool did not exit (timeout, not started, transfer)
	Message  string    `json:"message"`
}

// QC failure codes shared with the Skill's character-consistency reference.
const (
	QCFaceDrift     = "face_drift"
	QCWardrobeDrift = "wardrobe_drift"
	QCSetDrift      = "set_drift"
	QCCameraDrift   = "camera_drift"
	QCAudioMismatch = "audio_mismatch"
)

var qcCodes = []string{QCFaceDrift, QCWardrobeDrift, QCSetDrift, QCCameraDrift, QCAudioMismatch}

// QCResult is the human review of the probed output (automatic similarity is
// advisory only).
type QCResult struct {
	Passed    bool      `json:"passed"`
	Failures  []string  `json:"failures,omitempty"`
	Reviewer  string    `json:"reviewer"`
	CheckedAt time.Time `json:"checked_at"`
	Notes     string    `json:"notes,omitempty"`
	// SampledAt lists the checked timestamps (start, middle, end at least).
	SampledAt []float64 `json:"sampled_at_seconds"`
}

// VideoJob is one shot generation, possibly across several providers.
type VideoJob struct {
	ID              string          `json:"id"`
	IdempotencyKey  string          `json:"idempotency_key"`
	Revision        int             `json:"revision"`
	Request         Request         `json:"request"`
	Chain           []Capability    `json:"candidate_chain"`
	ChainIndex      int             `json:"chain_index"`
	Status          Status          `json:"status"`
	StatusReason    string          `json:"status_reason,omitempty"`
	CostCapMicroUSD int64           `json:"cost_cap_micro_usd"`
	Attempts        []Attempt       `json:"attempts"`
	Fallbacks       []FallbackEvent `json:"fallbacks"`
	Output          *Output         `json:"output,omitempty"`
	QC              *QCResult       `json:"qc,omitempty"`
	ProbeAttempts   int             `json:"probe_attempts,omitempty"`
	ProbeNextAt     *time.Time      `json:"probe_next_at,omitempty"`
	ProbeErrorKind  string          `json:"probe_error_kind,omitempty"`
	ProbeOutputURI  string          `json:"probe_output_uri,omitempty"`
	ProbeReviewedBy string          `json:"probe_reviewed_by,omitempty"`
	ProbeReviewNote string          `json:"probe_review_note,omitempty"`
	// ProbeLog keeps the most recent probe failures (exit code and a
	// redacted, truncated message) as evidence for the reviewer.
	ProbeLog []ProbeFailure `json:"probe_log,omitempty"`
	// ShotSpecRevision is the shot spec version the user confirmed.
	ShotSpecRevision int `json:"shot_spec_revision,omitempty"`
	// LegacyVideoGenerationID links the site's existing VideoGeneration row.
	LegacyVideoGenerationID string    `json:"legacy_video_generation_id,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// NewJob builds a queued job; it is blocked when no provider qualifies.
func NewJob(id, idempotencyKey string, req Request, caps []Capability, costCap int64, now time.Time) *VideoJob {
	chain, rejected := CandidateChain(caps, req)
	j := &VideoJob{ID: id, IdempotencyKey: idempotencyKey, Request: req, Chain: chain, Status: StatusQueued, CostCapMicroUSD: costCap, CreatedAt: now, UpdatedAt: now}
	if len(chain) == 0 {
		j.Status = StatusBlocked
		j.StatusReason = fmt.Sprintf("no enabled provider supports this shot: %v", rejected)
	}
	return j
}

// Current returns the latest attempt, or nil.
func (j *VideoJob) Current() *Attempt {
	if len(j.Attempts) == 0 {
		return nil
	}
	return &j.Attempts[len(j.Attempts)-1]
}

// TotalCostMicroUSD sums billed cost across attempts.
func (j *VideoJob) TotalCostMicroUSD() int64 {
	var total int64
	for _, a := range j.Attempts {
		total += a.CostMicroUSD
	}
	return total
}

// RecordProbe stores the ffprobe measurement and moves to QC, or fails the
// job when the file does not match the request.
func (j *VideoJob) RecordProbe(out Output, now time.Time) error {
	if j.Status != StatusProbing {
		return fmt.Errorf("job %s: probe recorded in status %s", j.ID, j.Status)
	}
	j.Output = &out
	j.UpdatedAt = now
	if len(out.ProbeFailures) > 0 {
		j.Status, j.StatusReason = StatusFailed, fmt.Sprintf("probe: %v", out.ProbeFailures)
		return nil
	}
	j.Status = StatusQCPending
	return nil
}

// RecordQC stores the review. A pass with no failure codes and three samples
// succeeds the job; any failure code marks the shot for redo.
func (j *VideoJob) RecordQC(qc QCResult) error {
	if j.Status != StatusQCPending || j.Output == nil {
		return fmt.Errorf("job %s: QC recorded in status %s", j.ID, j.Status)
	}
	if qc.Reviewer == "" {
		return errors.New("QC needs a reviewer")
	}
	for _, f := range qc.Failures {
		if !slices.Contains(qcCodes, f) {
			return fmt.Errorf("unknown QC failure code %q", f)
		}
	}
	if qc.Passed && len(qc.Failures) > 0 {
		return errors.New("QC cannot pass with failure codes")
	}
	if qc.Passed && len(qc.SampledAt) < 3 {
		return errors.New("QC pass needs at least start, middle and end samples")
	}
	j.QC = &qc
	j.UpdatedAt = qc.CheckedAt
	if qc.Passed {
		j.Status, j.StatusReason = StatusSucceeded, ""
	} else {
		j.Status, j.StatusReason = StatusFailed, fmt.Sprintf("qc: %v", qc.Failures)
	}
	return nil
}

// Store persists jobs. SaveJob must reject a stale Revision.
type Store interface {
	// CreateJob returns the existing job for the idempotency key, if any.
	CreateJob(ctx context.Context, j *VideoJob) (stored *VideoJob, created bool, err error)
	GetJob(ctx context.Context, id string) (*VideoJob, error)
	SaveJob(ctx context.Context, j *VideoJob) error
}

// ErrConflict means another worker saved the job first.
var ErrConflict = errors.New("videojob: revision conflict")

// ErrNotFound means no such job.
var ErrNotFound = errors.New("videojob: not found")

// MemStore is an in-memory Store for tests and local dry runs.
type MemStore struct {
	mu    sync.Mutex
	jobs  map[string]VideoJob
	byKey map[string]string
}

func NewMemStore() *MemStore {
	return &MemStore{jobs: map[string]VideoJob{}, byKey: map[string]string{}}
}

func clone(j VideoJob) *VideoJob {
	c := j
	c.Attempts = slices.Clone(j.Attempts)
	c.Fallbacks = slices.Clone(j.Fallbacks)
	c.Chain = slices.Clone(j.Chain)
	return &c
}

func (m *MemStore) CreateJob(_ context.Context, j *VideoJob) (*VideoJob, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byKey[j.IdempotencyKey]; ok {
		return clone(m.jobs[id]), false, nil
	}
	m.jobs[j.ID] = *clone(*j)
	m.byKey[j.IdempotencyKey] = j.ID
	return clone(*j), true, nil
}

func (m *MemStore) GetJob(_ context.Context, id string) (*VideoJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(j), nil
}

func (m *MemStore) SaveJob(_ context.Context, j *VideoJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.jobs[j.ID]
	if !ok {
		return ErrNotFound
	}
	if cur.Revision != j.Revision {
		return ErrConflict
	}
	j.Revision++
	m.jobs[j.ID] = *clone(*j)
	return nil
}
