package videojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// H3CLIConfig drives the minimax-h3-video Skill's `scripts/h3-cloud`
// command. `generate` starts/stops the GPU, submits to ComfyUI and downloads
// the MP4 in one run; this provider wraps that run as a resumable task.
type H3CLIConfig struct {
	Command    string  // absolute path to scripts/h3-cloud
	ConfigPath string  // worker-local runtime config (holds credentials; never sent to the site)
	Megapixels float64 // 0.2–1.0
	WorkRoot   string  // one directory per attempt
	// Manifest describes where the run records prompt_id and the MP4. The
	// default (manifest.json in the run directory with keys prompt_id and
	// output_path) is an assumption to confirm against Skill 0.2.0.
	ManifestFile string
	PromptIDKey  string
	OutputKey    string
	// ResumeArgs, if set, re-attaches to an existing prompt after a worker
	// restart, e.g. ["resume", "--config", "{config}", "--prompt-id", "{prompt_id}"].
	// The Skill notes describe resuming but not the command, so it is empty
	// by default and an orphaned prompt goes to needs_reconcile instead.
	ResumeArgs []string
	// StoppedMarker is a substring of `status` output that proves the GPU
	// instance is stopped; it is recorded as stop evidence.
	StoppedMarker      string
	HourlyRateMicroUSD int64
	MaxRuntime         time.Duration
}

// H3CLIProvider implements Provider over the H3 CLI. It runs only in the H3
// worker, never in the site's web process.
type H3CLIProvider struct {
	Cfg   H3CLIConfig
	Fetch *AssetFetcher
	// StoreOutput uploads the finished MP4 to the site's asset store.
	StoreOutput func(ctx context.Context, job *VideoJob, localPath string) (string, error)
	Now         func() time.Time

	mu   sync.Mutex
	runs map[string]*h3Run
}

type h3Run struct {
	cmd    *exec.Cmd
	done   chan struct{}
	err    error
	stderr bytes.Buffer
}

func (p *H3CLIProvider) ID() string { return "minimax-h3" }

func (p *H3CLIProvider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// runDir is per attempt; the runner appends the attempt before Submit.
func (p *H3CLIProvider) runDir(job *VideoJob) string {
	return filepath.Join(p.Cfg.WorkRoot, job.ID, fmt.Sprintf("attempt-%d", len(job.Attempts)-1))
}

// H3CLIArgs maps an input to `generate` flags. Reference images and key
// frames are mutually exclusive; there is no video input.
func H3CLIArgs(cfg H3CLIConfig, in H3Input, promptFile string, imagePaths []string) ([]string, error) {
	args := []string{"generate", "--config", cfg.ConfigPath,
		"--aspect-ratio", in.AspectRatio,
		"--duration-seconds", strconv.FormatFloat(in.DurationSeconds, 'f', -1, 64),
		"--megapixels", strconv.FormatFloat(cfg.Megapixels, 'f', -1, 64),
		"--prompt-file", promptFile}
	if len(imagePaths) != len(in.Images) {
		return nil, errors.New("image paths do not match inputs")
	}
	for i, img := range in.Images {
		switch {
		case in.Mode == ModeIdentityRefs:
			args = append(args, "--reference-image", imagePaths[i])
		case img.Purpose == "first_frame":
			args = append(args, "--first-frame-image", imagePaths[i])
		case img.Purpose == "last_frame":
			args = append(args, "--last-frame-image", imagePaths[i])
		default:
			return nil, &ProviderError{ClassInvalidInput, "H3 key-frame mode cannot take " + img.Purpose}
		}
	}
	if cfg.Megapixels < 0.2 || cfg.Megapixels > 1.0 {
		return nil, &ProviderError{ClassInvalidInput, "H3 megapixels must be 0.2–1.0"}
	}
	return args, nil
}

func (p *H3CLIProvider) start(dir string, args []string) {
	run := &h3Run{cmd: exec.Command(p.Cfg.Command, args...), done: make(chan struct{})}
	run.cmd.Dir = dir
	run.cmd.Stderr = &run.stderr
	if log, err := os.Create(filepath.Join(dir, "stdout.log")); err == nil {
		run.cmd.Stdout = log
	}
	p.mu.Lock()
	if p.runs == nil {
		p.runs = map[string]*h3Run{}
	}
	p.runs[dir] = run
	p.mu.Unlock()
	if err := run.cmd.Start(); err != nil {
		run.err = err
		close(run.done)
		return
	}
	go func() {
		run.err = run.cmd.Wait()
		if f, ok := run.cmd.Stdout.(*os.File); ok {
			f.Close()
		}
		close(run.done)
	}()
}

func (p *H3CLIProvider) Submit(ctx context.Context, job *VideoJob, att *Attempt) (string, error) {
	in, err := H3InputFor(job.Request, att.PromptEN)
	if err != nil {
		return "", err
	}
	dir := p.runDir(job)
	if _, err := os.Stat(dir); err == nil {
		// A previous worker got this far; never start a second paid run.
		return "", &ProviderError{ClassSubmitUnknown, "run directory already exists: " + dir}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", &ProviderError{ClassTransient, err.Error()}
	}
	var paths []string
	for i, img := range in.Images {
		ext := path.Ext(img.URI)
		if ext == "" || len(ext) > 5 {
			ext = ".png"
		}
		local, err := p.Fetch.Download(ctx, img, dir, fmt.Sprintf("%02d-%s%s", i, strings.ReplaceAll(img.Purpose, ":", "-"), ext))
		if err != nil {
			var pe *ProviderError
			if errors.As(err, &pe) {
				return "", pe
			}
			return "", &ProviderError{ClassTransient, err.Error()}
		}
		paths = append(paths, local)
	}
	promptFile := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte(in.PromptEN), 0o600); err != nil {
		return "", &ProviderError{ClassTransient, err.Error()}
	}
	args, err := H3CLIArgs(p.Cfg, in, promptFile, paths)
	if err != nil {
		return "", err
	}
	// Everything before this line is free; from here the GPU may bill.
	p.start(dir, args)
	return "h3run:pending", nil
}

func (p *H3CLIProvider) manifest(dir string) (promptID, output string) {
	name := p.Cfg.ManifestFile
	if name == "" {
		name = "manifest.json"
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", ""
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return "", ""
	}
	key := func(k, def string) string {
		if k == "" {
			k = def
		}
		s, _ := m[k].(string)
		return s
	}
	promptID, output = key(p.Cfg.PromptIDKey, "prompt_id"), key(p.Cfg.OutputKey, "output_path")
	if output != "" && !filepath.IsAbs(output) {
		output = filepath.Join(dir, output)
	}
	return promptID, output
}

func (p *H3CLIProvider) cost(att *Attempt) (int64, float64) {
	secs := p.now().Sub(att.StartedAt).Seconds()
	return int64(secs * float64(p.Cfg.HourlyRateMicroUSD) / 3600), secs
}

// stopEvidence runs the read-only `status` command and saves its output.
func (p *H3CLIProvider) stopEvidence(ctx context.Context, dir string) bool {
	out, err := exec.CommandContext(ctx, p.Cfg.Command, "status", "--config", p.Cfg.ConfigPath).CombinedOutput()
	_ = os.WriteFile(filepath.Join(dir, "status-after.log"), out, 0o600)
	return err == nil && p.Cfg.StoppedMarker != "" && strings.Contains(string(out), p.Cfg.StoppedMarker)
}

// Poll follows the local run (or a resumed one); it never starts `generate`.
func (p *H3CLIProvider) Poll(ctx context.Context, job *VideoJob, att *Attempt) (PollResult, error) {
	dir := p.runDir(job)
	res := PollResult{}
	res.CostMicroUSD, res.GPUSeconds = p.cost(att)
	promptID, output := p.manifest(dir)
	if promptID != "" {
		att.ExternalTaskID = promptID
	}

	p.mu.Lock()
	run := p.runs[dir]
	p.mu.Unlock()
	if run == nil {
		if output != "" && fileNonEmpty(output) {
			return p.finish(ctx, job, dir, output, res)
		}
		if promptID == "" || len(p.Cfg.ResumeArgs) == 0 {
			return res, fmt.Errorf("%w: run %s has no live process (prompt_id %q); check `h3-cloud status`", ErrResumeUnavailable, dir, promptID)
		}
		args := make([]string, len(p.Cfg.ResumeArgs))
		for i, a := range p.Cfg.ResumeArgs {
			args[i] = strings.NewReplacer("{config}", p.Cfg.ConfigPath, "{prompt_id}", promptID).Replace(a)
		}
		p.start(dir, args)
		res.State = TaskPending
		return res, nil
	}

	select {
	case <-run.done:
	default:
		if p.Cfg.MaxRuntime > 0 && p.now().Sub(att.StartedAt) > p.Cfg.MaxRuntime {
			// Killing the CLI could leave the GPU running; hand it to a person.
			return res, fmt.Errorf("%w: run %s exceeded %s; check the instance before acting", ErrResumeUnavailable, dir, p.Cfg.MaxRuntime)
		}
		res.State = TaskPending
		return res, nil
	}
	if run.err != nil {
		_ = os.WriteFile(filepath.Join(dir, "stderr.log"), run.stderr.Bytes(), 0o600)
		res.InstanceStopped = p.stopEvidence(ctx, dir)
		// Whether the prompt ran (and billed) is unknown; do not resubmit.
		return res, fmt.Errorf("%w: h3-cloud exited with %v (prompt_id %q)", ErrResumeUnavailable, run.err, promptID)
	}
	if output == "" || !fileNonEmpty(output) {
		res.State, res.Err = TaskFailed, &ProviderError{ClassTaskFailed, "h3-cloud finished without an MP4 in its manifest"}
		res.InstanceStopped = p.stopEvidence(ctx, dir)
		return res, nil
	}
	return p.finish(ctx, job, dir, output, res)
}

func (p *H3CLIProvider) finish(ctx context.Context, job *VideoJob, dir, output string, res PollResult) (PollResult, error) {
	uri, err := p.StoreOutput(ctx, job, output)
	if err != nil {
		return PollResult{}, fmt.Errorf("store H3 output: %w", err)
	}
	res.State, res.OutputURI = TaskSucceeded, uri
	res.InstanceStopped = p.stopEvidence(ctx, dir)
	return res, nil
}

func fileNonEmpty(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}
