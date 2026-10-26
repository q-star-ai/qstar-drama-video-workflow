package videojob

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrorClass decides whether a failure switches provider.
type ErrorClass string

const (
	// ClassInvalidInput: the request itself is wrong; switching won't help.
	ClassInvalidInput ErrorClass = "invalid_input"
	// ClassContentSafety: may get one safety-prompt retry on the same provider.
	ClassContentSafety ErrorClass = "content_safety"
	ClassCapacity      ErrorClass = "capacity"
	ClassTransient     ErrorClass = "transient"
	ClassTaskFailed    ErrorClass = "task_failed"
	ClassTimeout       ErrorClass = "timeout"
	// ClassSubmitUnknown: the submit call may or may not have created a paid
	// task. Never resubmit; reconcile first.
	ClassSubmitUnknown ErrorClass = "submit_unknown"
)

// ProviderError is a classified provider failure.
type ProviderError struct {
	Class   ErrorClass
	Message string
}

func (e *ProviderError) Error() string { return fmt.Sprintf("%s: %s", e.Class, e.Message) }

// TaskState is the provider-side state of a submitted task.
type TaskState string

const (
	TaskPending   TaskState = "pending"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
)

// PollResult is one status check of an external task.
type PollResult struct {
	State        TaskState
	OutputURI    string
	CostMicroUSD int64
	GPUSeconds   float64
	Err          *ProviderError
	// InstanceStopped is set when a GPU provider shut down its own instance.
	InstanceStopped bool
}

// Provider submits and polls one external video service.
type Provider interface {
	ID() string
	// Submit starts a task and returns its external ID. It may set
	// att.InstanceID and att.CostMicroUSD.
	Submit(ctx context.Context, job *VideoJob, att *Attempt) (string, error)
	// Poll checks an existing task; it must never submit.
	Poll(ctx context.Context, job *VideoJob, att *Attempt) (PollResult, error)
}

// Reconciler is optional: it finds a task a crashed submit may have created.
type Reconciler interface {
	FindTask(ctx context.Context, job *VideoJob, att *Attempt) (externalID string, found bool, err error)
}

// Runner advances jobs one step at a time. It runs in the worker, never in a
// web request.
type Runner struct {
	Store     Store
	Providers map[string]Provider
	// SafetyRewrite, when set, rewrites a prompt for one retry after a content
	// safety rejection. The retry is recorded as its own attempt.
	SafetyRewrite func(prompt string) string
	Now           func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// Step loads the job, performs at most one submit or poll, and saves it.
func (r *Runner) Step(ctx context.Context, id string) (*VideoJob, error) {
	j, err := r.Store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.Status != StatusQueued && j.Status != StatusRunning {
		return j, nil
	}
	cur := j.Current()
	switch {
	case cur == nil || cur.Status == AttemptFailed:
		return j, r.submit(ctx, j, "submit", j.Request.PromptEN)
	case cur.Status == AttemptSubmitting:
		return j, r.reconcile(ctx, j, cur)
	case cur.Status == AttemptRunning:
		return j, r.poll(ctx, j, cur)
	default:
		return j, fmt.Errorf("job %s: inconsistent attempt state %s", j.ID, cur.Status)
	}
}

func (r *Runner) provider(j *VideoJob) (Capability, Provider, error) {
	c := j.Chain[j.ChainIndex]
	p, ok := r.Providers[c.Provider]
	if !ok {
		return c, nil, fmt.Errorf("provider %s not registered in this worker", c.Provider)
	}
	return c, p, nil
}

func (r *Runner) submit(ctx context.Context, j *VideoJob, kind, prompt string) error {
	c, p, err := r.provider(j)
	if err != nil {
		return err
	}
	if j.CostCapMicroUSD > 0 && j.TotalCostMicroUSD() >= j.CostCapMicroUSD {
		j.Status, j.StatusReason = StatusBlocked, "cost cap reached before next submit"
		return r.save(ctx, j)
	}
	j.Attempts = append(j.Attempts, Attempt{Provider: c.Provider, Model: c.Model, Kind: kind, PromptEN: prompt, Status: AttemptSubmitting, StartedAt: r.now()})
	j.Status = StatusRunning
	// Persist intent first: if we crash after the provider accepted the task,
	// the next Step sees "submitting" and reconciles instead of paying again.
	if err := r.save(ctx, j); err != nil {
		return err
	}
	att := j.Current()
	externalID, err := p.Submit(ctx, j, att)
	if err != nil {
		var pe *ProviderError
		if !errors.As(err, &pe) {
			pe = &ProviderError{Class: ClassSubmitUnknown, Message: err.Error()}
		}
		if pe.Class == ClassSubmitUnknown {
			j.Status, j.StatusReason = StatusNeedsReconcile, "submit outcome unknown: "+pe.Message
			return r.save(ctx, j)
		}
		return r.fail(ctx, j, att, pe)
	}
	att.ExternalTaskID, att.Status = externalID, AttemptRunning
	return r.save(ctx, j)
}

func (r *Runner) reconcile(ctx context.Context, j *VideoJob, att *Attempt) error {
	_, p, err := r.provider(j)
	if err != nil {
		return err
	}
	if rec, ok := p.(Reconciler); ok {
		id, found, err := rec.FindTask(ctx, j, att)
		if err == nil && found {
			att.ExternalTaskID, att.Status = id, AttemptRunning
			return r.save(ctx, j)
		}
	}
	j.Status, j.StatusReason = StatusNeedsReconcile, "worker stopped during submit; check the provider console before resubmitting"
	return r.save(ctx, j)
}

func (r *Runner) poll(ctx context.Context, j *VideoJob, att *Attempt) error {
	_, p, err := r.provider(j)
	if err != nil {
		return err
	}
	res, err := p.Poll(ctx, j, att)
	if err != nil {
		// Status check failed; the task is still there. Try again next Step.
		return err
	}
	att.CostMicroUSD, att.GPUSeconds = max(att.CostMicroUSD, res.CostMicroUSD), max(att.GPUSeconds, res.GPUSeconds)
	att.InstanceStopped = att.InstanceStopped || res.InstanceStopped
	switch res.State {
	case TaskPending:
		return r.save(ctx, j)
	case TaskSucceeded:
		att.Status, att.OutputURI, att.FinishedAt = AttemptSucceeded, res.OutputURI, r.now()
		j.Status = StatusProbing
		return r.save(ctx, j)
	default:
		pe := res.Err
		if pe == nil {
			pe = &ProviderError{Class: ClassTaskFailed, Message: "provider reported failure"}
		}
		return r.fail(ctx, j, att, pe)
	}
}

// fail closes the attempt and decides: safety retry, switch, or stop.
func (r *Runner) fail(ctx context.Context, j *VideoJob, att *Attempt, pe *ProviderError) error {
	att.Status, att.ErrorClass, att.Error, att.FinishedAt = AttemptFailed, pe.Class, pe.Message, r.now()
	if pe.Class == ClassInvalidInput {
		j.Status, j.StatusReason = StatusBlocked, pe.Error()
		return r.save(ctx, j)
	}
	if pe.Class == ClassContentSafety && r.SafetyRewrite != nil && att.Kind != "safety_retry" {
		if err := r.save(ctx, j); err != nil {
			return err
		}
		return r.submit(ctx, j, "safety_retry", r.SafetyRewrite(att.PromptEN))
	}
	if j.ChainIndex+1 >= len(j.Chain) {
		j.Status, j.StatusReason = StatusFailed, "all candidate providers failed; last: "+pe.Error()
		return r.save(ctx, j)
	}
	from := j.Chain[j.ChainIndex].Provider
	j.ChainIndex++
	j.Fallbacks = append(j.Fallbacks, FallbackEvent{From: from, To: j.Chain[j.ChainIndex].Provider, Class: pe.Class, Reason: pe.Message, At: r.now()})
	if err := r.save(ctx, j); err != nil {
		return err
	}
	return r.submit(ctx, j, "submit", j.Request.PromptEN)
}

func (r *Runner) save(ctx context.Context, j *VideoJob) error {
	j.UpdatedAt = r.now()
	return r.Store.SaveJob(ctx, j)
}

// ResolveReconcile is the operator action after checking the provider console:
// attach the found task ID, or confirm none exists so the job may resubmit.
func ResolveReconcile(j *VideoJob, externalID string) error {
	att := j.Current()
	if j.Status != StatusNeedsReconcile || att == nil || att.Status != AttemptSubmitting {
		return fmt.Errorf("job %s is not awaiting reconcile", j.ID)
	}
	if externalID != "" {
		j.Status, j.StatusReason = StatusRunning, ""
		att.ExternalTaskID, att.Status = externalID, AttemptRunning
		return nil
	}
	// Keep the record; the next Step resubmits on the same provider.
	j.Status, j.StatusReason = StatusQueued, ""
	att.Status, att.ErrorClass, att.Error = AttemptFailed, ClassSubmitUnknown, "operator confirmed no task was created"
	return nil
}
