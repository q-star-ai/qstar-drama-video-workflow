// Package videojob is the unified short-drama video job model: provider
// capabilities, reference binding, routing, resumable attempts, output probing
// and QC. It performs no network or GPU calls itself; providers do.
package videojob

import (
	"fmt"
	"slices"
)

// InputMode is how a shot conditions the video model.
type InputMode string

const (
	// ModeIdentityRefs sends approved face, wardrobe and set images.
	ModeIdentityRefs InputMode = "identity_refs"
	// ModeFirstLast sends a previs first frame and optional last frame.
	ModeFirstLast InputMode = "first_last"
	// ModeVideoRef sends a previs greybox clip plus identity images. Only
	// providers whose video-reference input has been verified may declare it.
	ModeVideoRef InputMode = "video_ref"
)

// ModeLimits bounds the images a provider accepts in one input mode.
type ModeLimits struct {
	MinImages int `json:"min_images"`
	MaxImages int `json:"max_images"`
	// AcceptsIdentityImages reports whether separate face/wardrobe images can
	// accompany this mode. H3 first_last cannot.
	AcceptsIdentityImages bool `json:"accepts_identity_images"`
	AcceptsVideo          bool `json:"accepts_video"`
}

// Capability is the server-side truth the UI form and submit validation use.
type Capability struct {
	Provider     string                   `json:"provider"`
	Model        string                   `json:"model"`
	Enabled      bool                     `json:"enabled"`
	Modes        map[InputMode]ModeLimits `json:"modes"`
	MinSeconds   float64                  `json:"min_seconds"`
	MaxSeconds   float64                  `json:"max_seconds"`
	AspectRatios []string                 `json:"aspect_ratios"`
	// Verification is "candidate" until a real sample passed QC on this site.
	Verification string `json:"verification_status"`
	// Fallback marks a last-resort channel tried after the primaries.
	Fallback bool `json:"fallback"`
	// CostPerSecondMicroUSD is an estimate for planning; attempts record the
	// actual billed cost.
	CostPerSecondMicroUSD int64  `json:"cost_per_second_micro_usd,omitempty"`
	Notes                 string `json:"notes,omitempty"`
}

// H3Capability describes the current MiniMax H3 GPU runtime: reference-image
// mode and first/last-frame mode are mutually exclusive, and there is no
// greybox video input. Model and cost come from the worker's runtime profile.
func H3Capability(model string, enabled bool) Capability {
	return Capability{
		Provider: "minimax-h3",
		Model:    model,
		Enabled:  enabled,
		Modes: map[InputMode]ModeLimits{
			ModeIdentityRefs: {MinImages: 1, MaxImages: 9, AcceptsIdentityImages: true},
			ModeFirstLast:    {MinImages: 1, MaxImages: 2},
		},
		MinSeconds:   2,
		MaxSeconds:   15,
		AspectRatios: []string{"9:16", "16:9", "1:1"},
		Verification: "candidate",
		Fallback:     true,
		Notes:        "identity_refs and first_last are mutually exclusive; no greybox video input",
	}
}

// Supports reports whether c can run req, and why not.
func (c Capability) Supports(req Request) (bool, string) {
	if !c.Enabled {
		return false, "provider disabled"
	}
	limits, ok := c.Modes[req.InputMode]
	if !ok {
		return false, fmt.Sprintf("input mode %s not supported", req.InputMode)
	}
	if req.DurationSeconds < c.MinSeconds || req.DurationSeconds > c.MaxSeconds {
		return false, fmt.Sprintf("duration %.1fs outside %.1f–%.1fs", req.DurationSeconds, c.MinSeconds, c.MaxSeconds)
	}
	if !slices.Contains(c.AspectRatios, req.AspectRatio) {
		return false, fmt.Sprintf("aspect ratio %s not supported", req.AspectRatio)
	}
	images, videos, identity := 0, 0, 0
	for _, ref := range req.References {
		switch ref.Kind() {
		case RefVideo:
			videos++
		case RefIdentity:
			identity++
			images++
		default:
			images++
		}
	}
	if videos > 0 && !limits.AcceptsVideo {
		return false, "video reference input not supported"
	}
	if identity > 0 && !limits.AcceptsIdentityImages {
		return false, "identity images cannot accompany this mode"
	}
	if images < limits.MinImages || images > limits.MaxImages {
		return false, fmt.Sprintf("%d images outside %d–%d", images, limits.MinImages, limits.MaxImages)
	}
	return true, ""
}

// CandidateChain orders the providers that can run req: verified primaries,
// candidate primaries, then fallbacks. Rejections explain the rest.
func CandidateChain(caps []Capability, req Request) (chain []Capability, rejected map[string]string) {
	rejected = map[string]string{}
	for _, c := range caps {
		if ok, why := c.Supports(req); ok {
			chain = append(chain, c)
		} else {
			rejected[c.Provider] = why
		}
	}
	rank := func(c Capability) int {
		r := 0
		if c.Fallback {
			r += 2
		}
		if c.Verification != "verified" {
			r++
		}
		return r
	}
	slices.SortStableFunc(chain, func(a, b Capability) int { return rank(a) - rank(b) })
	return chain, rejected
}
