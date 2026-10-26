package videojob

import (
	"errors"
	"fmt"
	"strings"
)

// AssetRef pins a customer-approved or previs asset by ID, version and
// checksum. URI is a private asset URI; signed URLs are minted at submit time
// by the worker and never stored on the job.
type AssetRef struct {
	AssetID string `json:"asset_id"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	URI     string `json:"uri"`
	// Purpose is identity:<character>, wardrobe:<character>, environment,
	// first_frame, last_frame or greybox.
	Purpose string `json:"purpose"`
}

// RefKind groups purposes for capability checks.
type RefKind int

const (
	RefImage RefKind = iota
	RefIdentity
	RefVideo
)

// Kind classifies the reference by purpose.
func (r AssetRef) Kind() RefKind {
	switch {
	case r.Purpose == "greybox":
		return RefVideo
	case strings.HasPrefix(r.Purpose, "identity:"), strings.HasPrefix(r.Purpose, "wardrobe:"):
		return RefIdentity
	default:
		return RefImage
	}
}

// Character binds a cast member to an approved face asset.
type Character struct {
	ID       string   `json:"id"`
	Identity AssetRef `json:"identity"`
}

// Previs holds optional CozyClay (or similar) previsualisation exports. They
// fix blocking, action and camera; they never replace face or wardrobe.
type Previs struct {
	FirstFrame *AssetRef `json:"first_frame,omitempty"`
	LastFrame  *AssetRef `json:"last_frame,omitempty"`
	Greybox    *AssetRef `json:"greybox,omitempty"`
	// Camera is the exported camera data (lens, position, path) as recorded.
	Camera map[string]any `json:"camera,omitempty"`
	// ReviewedBy is set once a person approved the previs for generation.
	ReviewedBy string `json:"reviewed_by,omitempty"`
}

// Shot is one storyboard row as the site stores it.
type Shot struct {
	EpisodeID       string              `json:"episode_id"`
	ShotID          string              `json:"shot_id"`
	DurationSeconds float64             `json:"duration_seconds"`
	AspectRatio     string              `json:"aspect_ratio"`
	Cast            []Character         `json:"cast"`
	Wardrobe        map[string]AssetRef `json:"wardrobe_by_character"`
	Environment     AssetRef            `json:"environment"`
	InputMode       InputMode           `json:"input_mode"`
	VisualEN        string              `json:"visual_en"`
	ActionEN        string              `json:"action_en"`
	CameraEN        string              `json:"camera_en"`
	DialogueZH      string              `json:"dialogue_zh"`
	Previs          Previs              `json:"previs"`
}

// Request is the provider-neutral generation input built from a Shot.
type Request struct {
	EpisodeID       string         `json:"episode_id"`
	ShotID          string         `json:"shot_id"`
	DurationSeconds float64        `json:"duration_seconds"`
	AspectRatio     string         `json:"aspect_ratio"`
	InputMode       InputMode      `json:"input_mode"`
	References      []AssetRef     `json:"references"`
	PromptEN        string         `json:"prompt_en"`
	DialogueZH      string         `json:"dialogue_zh"`
	Camera          map[string]any `json:"camera,omitempty"`
	// IdentityRisk is set when the mode cannot carry the approved face, so a
	// person must verify the face in the output.
	IdentityRisk string `json:"identity_risk,omitempty"`
}

const continuityClause = "Preserve the exact character identity, wardrobe, set layout and camera axis established by the supplied references. No new people, no extra limbs, no costume changes, no on-screen text."

func checkRef(ref AssetRef, label string) error {
	if ref.AssetID == "" || ref.URI == "" || ref.Version == "" || ref.SHA256 == "" {
		return fmt.Errorf("%s: asset_id, uri, version and sha256 are required", label)
	}
	if strings.HasPrefix(ref.URI, "http://") || strings.HasPrefix(ref.URI, "https://") {
		return fmt.Errorf("%s: store a private asset URI, not a web URL", label)
	}
	return nil
}

// BuildRequest validates a shot and binds its references for its input mode.
func BuildRequest(s Shot) (Request, error) {
	if s.EpisodeID == "" || s.ShotID == "" {
		return Request{}, errors.New("episode_id and shot_id are required")
	}
	if s.DurationSeconds < 5 || s.DurationSeconds > 10 {
		return Request{}, fmt.Errorf("shot %s: duration must be 5–10 seconds", s.ShotID)
	}
	switch s.AspectRatio {
	case "9:16", "16:9", "1:1":
	default:
		return Request{}, fmt.Errorf("shot %s: unsupported aspect ratio %q", s.ShotID, s.AspectRatio)
	}
	for _, f := range []struct{ v, name string }{{s.VisualEN, "visual_en"}, {s.ActionEN, "action_en"}, {s.CameraEN, "camera_en"}} {
		if strings.TrimSpace(f.v) == "" {
			return Request{}, fmt.Errorf("shot %s: %s is required", s.ShotID, f.name)
		}
	}
	seen := map[string]bool{}
	for _, c := range s.Cast {
		if seen[c.ID] {
			return Request{}, fmt.Errorf("shot %s: duplicate character %s", s.ShotID, c.ID)
		}
		seen[c.ID] = true
		if err := checkRef(c.Identity, "identity "+c.ID); err != nil {
			return Request{}, err
		}
		w, ok := s.Wardrobe[c.ID]
		if !ok {
			return Request{}, fmt.Errorf("shot %s: character %s needs a wardrobe asset", s.ShotID, c.ID)
		}
		if err := checkRef(w, "wardrobe "+c.ID); err != nil {
			return Request{}, err
		}
	}
	if len(s.Wardrobe) != len(s.Cast) {
		return Request{}, fmt.Errorf("shot %s: wardrobe given for a character not in the shot", s.ShotID)
	}
	if err := checkRef(s.Environment, "environment"); err != nil {
		return Request{}, err
	}

	req := Request{
		EpisodeID: s.EpisodeID, ShotID: s.ShotID,
		DurationSeconds: s.DurationSeconds, AspectRatio: s.AspectRatio,
		InputMode: s.InputMode, DialogueZH: s.DialogueZH, Camera: s.Previs.Camera,
		PromptEN: strings.Join([]string{strings.TrimSpace(s.VisualEN), strings.TrimSpace(s.ActionEN), strings.TrimSpace(s.CameraEN), continuityClause}, " "),
	}
	identityRefs := func() {
		for _, c := range s.Cast {
			id, w := c.Identity, s.Wardrobe[c.ID]
			id.Purpose, w.Purpose = "identity:"+c.ID, "wardrobe:"+c.ID
			req.References = append(req.References, id, w)
		}
		env := s.Environment
		env.Purpose = "environment"
		req.References = append(req.References, env)
	}
	previsRef := func(ref *AssetRef, purpose string) error {
		if err := checkRef(*ref, purpose); err != nil {
			return err
		}
		r := *ref
		r.Purpose = purpose
		req.References = append(req.References, r)
		return nil
	}
	needsReview := func() error {
		if s.Previs.ReviewedBy == "" {
			return fmt.Errorf("shot %s: previs must be reviewed by a person before generation", s.ShotID)
		}
		return nil
	}

	switch s.InputMode {
	case ModeIdentityRefs:
		identityRefs()
	case ModeFirstLast:
		if s.Previs.FirstFrame == nil {
			return Request{}, fmt.Errorf("shot %s: first_last requires a first frame", s.ShotID)
		}
		if err := needsReview(); err != nil {
			return Request{}, err
		}
		if err := previsRef(s.Previs.FirstFrame, "first_frame"); err != nil {
			return Request{}, err
		}
		if s.Previs.LastFrame != nil {
			if err := previsRef(s.Previs.LastFrame, "last_frame"); err != nil {
				return Request{}, err
			}
		}
		if len(s.Cast) > 0 {
			req.IdentityRisk = "first_last carries no separate face/wardrobe images; verify face and wardrobe in the output"
		}
	case ModeVideoRef:
		if s.Previs.Greybox == nil {
			return Request{}, fmt.Errorf("shot %s: video_ref requires a greybox clip", s.ShotID)
		}
		if err := needsReview(); err != nil {
			return Request{}, err
		}
		identityRefs()
		if err := previsRef(s.Previs.Greybox, "greybox"); err != nil {
			return Request{}, err
		}
	default:
		return Request{}, fmt.Errorf("shot %s: unknown input mode %q", s.ShotID, s.InputMode)
	}
	return req, nil
}
