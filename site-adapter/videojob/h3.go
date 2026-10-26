package videojob

import (
	"context"
	"fmt"
	"time"
)

// H3Input is what the H3 worker submits. Images carry private asset URIs; the
// runtime mints short-lived download URLs on the GPU side.
type H3Input struct {
	Mode            InputMode  `json:"mode"`
	Images          []AssetRef `json:"images"`
	PromptEN        string     `json:"prompt_en"`
	DurationSeconds float64    `json:"duration_seconds"`
	AspectRatio     string     `json:"aspect_ratio"`
	// ClientTag lets the runtime find a prompt after a crashed submit.
	ClientTag string `json:"client_tag"`
}

// H3Lease says which job started an instance; only that job may stop it.
type H3Lease struct {
	InstanceID string
	OwnerJobID string
	StartedAt  time.Time
}

// H3PromptStatus is the runtime's view of one prompt_id.
type H3PromptStatus struct {
	State TaskState
	Error string
}

// H3Runtime wraps the installed minimax-h3-video tooling (instance start,
// prompt submit, status, download, stop). It lives only in the worker, with
// credentials in the worker's restricted config.
type H3Runtime interface {
	// AcquireInstance reuses a running instance or starts one owned by jobID.
	AcquireInstance(ctx context.Context, jobID string) (H3Lease, error)
	Lease(ctx context.Context, instanceID string) (H3Lease, error)
	Submit(ctx context.Context, instanceID string, in H3Input) (promptID string, err error)
	Status(ctx context.Context, instanceID, promptID string) (H3PromptStatus, error)
	// StoreOutput downloads the result into the site's asset store.
	StoreOutput(ctx context.Context, instanceID, promptID string, job *VideoJob) (assetURI string, err error)
	ActivePrompts(ctx context.Context, instanceID string) (int, error)
	Stop(ctx context.Context, instanceID string) error
}

// H3PromptFinder is optional: look up a prompt by ClientTag on any instance
// after a crash (the instance ID may not have been saved yet).
type H3PromptFinder interface {
	FindPrompt(ctx context.Context, clientTag string) (instanceID, promptID string, found bool, err error)
}

// H3Provider is the resumable MiniMax H3 GPU fallback channel.
type H3Provider struct {
	Runtime            H3Runtime
	HourlyRateMicroUSD int64
	MaxRuntime         time.Duration
	Now                func() time.Time
}

func (h *H3Provider) ID() string { return "minimax-h3" }

func (h *H3Provider) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

func clientTag(job *VideoJob) string { return fmt.Sprintf("%s#%d", job.ID, len(job.Attempts)) }

// H3InputFor maps a request to H3, refusing combinations H3 cannot run.
func H3InputFor(req Request, prompt string) (H3Input, error) {
	in := H3Input{Mode: req.InputMode, PromptEN: prompt, DurationSeconds: req.DurationSeconds, AspectRatio: req.AspectRatio}
	switch req.InputMode {
	case ModeIdentityRefs:
		for _, r := range req.References {
			if r.Kind() == RefVideo {
				return in, &ProviderError{ClassInvalidInput, "H3 has no greybox video input"}
			}
			if r.Purpose == "first_frame" || r.Purpose == "last_frame" {
				return in, &ProviderError{ClassInvalidInput, "H3 reference-image mode cannot also take first/last frames"}
			}
			in.Images = append(in.Images, r)
		}
		if len(in.Images) < 1 || len(in.Images) > 9 {
			return in, &ProviderError{ClassInvalidInput, fmt.Sprintf("H3 takes 1–9 reference images, got %d", len(in.Images))}
		}
	case ModeFirstLast:
		for _, r := range req.References {
			if r.Purpose != "first_frame" && r.Purpose != "last_frame" {
				return in, &ProviderError{ClassInvalidInput, "H3 first/last-frame mode cannot also take " + r.Purpose + " references"}
			}
			in.Images = append(in.Images, r)
		}
		if len(in.Images) == 0 || in.Images[0].Purpose != "first_frame" {
			return in, &ProviderError{ClassInvalidInput, "H3 first/last-frame mode needs a first frame"}
		}
	default:
		return in, &ProviderError{ClassInvalidInput, fmt.Sprintf("H3 does not support input mode %s", req.InputMode)}
	}
	return in, nil
}

func (h *H3Provider) Submit(ctx context.Context, job *VideoJob, att *Attempt) (string, error) {
	in, err := H3InputFor(job.Request, att.PromptEN)
	if err != nil {
		return "", err
	}
	in.ClientTag = clientTag(job)
	lease, err := h.Runtime.AcquireInstance(ctx, job.ID)
	if err != nil {
		// No prompt was submitted, so nothing was billed for generation.
		return "", &ProviderError{ClassCapacity, "H3 instance unavailable: " + err.Error()}
	}
	att.InstanceID = lease.InstanceID
	promptID, err := h.Runtime.Submit(ctx, lease.InstanceID, in)
	if err != nil {
		if pe, ok := err.(*ProviderError); ok {
			if pe.Class != ClassSubmitUnknown {
				att.InstanceStopped = h.stopIfOwner(ctx, job, att)
			}
			return "", pe
		}
		return "", &ProviderError{ClassSubmitUnknown, err.Error()}
	}
	return promptID, nil
}

// FindTask implements Reconciler using the prompt's ClientTag.
func (h *H3Provider) FindTask(ctx context.Context, job *VideoJob, att *Attempt) (string, bool, error) {
	finder, ok := h.Runtime.(H3PromptFinder)
	if !ok {
		return "", false, nil
	}
	instanceID, promptID, found, err := finder.FindPrompt(ctx, clientTag(job))
	if found {
		att.InstanceID = instanceID
	}
	return promptID, found, err
}

func (h *H3Provider) cost(att *Attempt) (int64, float64) {
	secs := h.now().Sub(att.StartedAt).Seconds()
	return int64(secs * float64(h.HourlyRateMicroUSD) / 3600), secs
}

// Poll queries the existing prompt_id; it never resubmits.
func (h *H3Provider) Poll(ctx context.Context, job *VideoJob, att *Attempt) (PollResult, error) {
	st, err := h.Runtime.Status(ctx, att.InstanceID, att.ExternalTaskID)
	if err != nil {
		return PollResult{}, err
	}
	res := PollResult{}
	res.CostMicroUSD, res.GPUSeconds = h.cost(att)
	switch st.State {
	case TaskPending:
		if h.MaxRuntime > 0 && h.now().Sub(att.StartedAt) > h.MaxRuntime {
			res.State, res.Err = TaskFailed, &ProviderError{ClassTimeout, fmt.Sprintf("H3 prompt exceeded %s", h.MaxRuntime)}
			res.InstanceStopped = h.stopIfOwner(ctx, job, att)
			return res, nil
		}
		res.State = TaskPending
		return res, nil
	case TaskSucceeded:
		uri, err := h.Runtime.StoreOutput(ctx, att.InstanceID, att.ExternalTaskID, job)
		if err != nil {
			// The output stays on the instance; retry the download next Step.
			return PollResult{}, fmt.Errorf("store H3 output: %w", err)
		}
		res.State, res.OutputURI = TaskSucceeded, uri
	default:
		res.State, res.Err = TaskFailed, &ProviderError{ClassTaskFailed, st.Error}
	}
	res.InstanceStopped = h.stopIfOwner(ctx, job, att)
	return res, nil
}

// stopIfOwner stops the instance only when this job started it and nothing
// else is running on it.
func (h *H3Provider) stopIfOwner(ctx context.Context, job *VideoJob, att *Attempt) bool {
	if att.InstanceID == "" {
		return false
	}
	lease, err := h.Runtime.Lease(ctx, att.InstanceID)
	if err != nil || lease.OwnerJobID != job.ID {
		return false
	}
	if n, err := h.Runtime.ActivePrompts(ctx, att.InstanceID); err != nil || n > 0 {
		return false
	}
	return h.Runtime.Stop(ctx, att.InstanceID) == nil
}
