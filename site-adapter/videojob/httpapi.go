package videojob

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// API exposes capabilities, planning and idempotent job creation. It only
// enqueues; workers run jobs. No provider keys are read or returned here.
type API struct {
	Store        Store
	Capabilities func(ctx context.Context) []Capability
	NewID        func() string
	CostCap      int64
	Now          func() time.Time
}

// Routes mounts the handlers under prefix (for example "/api/video").
func (a *API) Routes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/capabilities", a.capabilities)
	mux.HandleFunc("POST "+prefix+"/plan", a.plan)
	mux.HandleFunc("POST "+prefix+"/jobs", a.createJob)
	mux.HandleFunc("GET "+prefix+"/jobs/{id}", a.getJob)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (a *API) capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": a.Capabilities(r.Context())})
}

// PlanResponse is what the storyboard UI renders before the user confirms.
type PlanResponse struct {
	Request  Request           `json:"request"`
	Chain    []Capability      `json:"candidate_chain"`
	Rejected map[string]string `json:"rejected"`
	// EstimatedMaxCostMicroUSD assumes every candidate runs once.
	EstimatedMaxCostMicroUSD int64 `json:"estimated_max_cost_micro_usd"`
}

func (a *API) planFor(r *http.Request) (PlanResponse, error) {
	var shot Shot
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&shot); err != nil {
		return PlanResponse{}, err
	}
	req, err := BuildRequest(shot)
	if err != nil {
		return PlanResponse{}, err
	}
	chain, rejected := CandidateChain(a.Capabilities(r.Context()), req)
	resp := PlanResponse{Request: req, Chain: chain, Rejected: rejected}
	for _, c := range chain {
		resp.EstimatedMaxCostMicroUSD += c.CostPerSecondMicroUSD * int64(req.DurationSeconds)
	}
	return resp, nil
}

func (a *API) plan(w http.ResponseWriter, r *http.Request) {
	resp, err := a.planFor(r)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) createJob(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("Idempotency-Key header is required"))
		return
	}
	plan, err := a.planFor(r)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	now := time.Now().UTC()
	if a.Now != nil {
		now = a.Now()
	}
	job := NewJob(a.NewID(), key, plan.Request, a.Capabilities(r.Context()), a.CostCap, now)
	stored, created, err := a.Store.CreateJob(r.Context(), job)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	code := http.StatusAccepted
	if !created {
		code = http.StatusOK
	}
	writeJSON(w, code, stored)
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.Store.GetJob(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}
