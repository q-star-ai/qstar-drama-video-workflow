package videojob

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseFFprobe reads `ffprobe -v error -print_format json -show_format
// -show_streams <file>` output.
func ParseFFprobe(data []byte, assetURI string) (Output, error) {
	var probe struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			AvgFrameRate string `json:"avg_frame_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
		} `json:"format"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Output{}, fmt.Errorf("parse ffprobe: %w", err)
	}
	out := Output{AssetURI: assetURI}
	out.DurationSeconds, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	out.SizeBytes, _ = strconv.ParseInt(probe.Format.Size, 10, 64)
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if out.VideoCodec == "" {
				out.VideoCodec, out.Width, out.Height = s.CodecName, s.Width, s.Height
				if num, den, ok := strings.Cut(s.AvgFrameRate, "/"); ok {
					n, _ := strconv.ParseFloat(num, 64)
					d, _ := strconv.ParseFloat(den, 64)
					if d > 0 {
						out.FPS = math.Round(n/d*100) / 100
					}
				}
			}
		case "audio":
			out.HasAudio = true
		}
	}
	return out, nil
}

// CheckOutput compares the probed file with the request and fills
// ProbeFailures. requireAudio is set when the shot must carry sound.
func CheckOutput(req Request, out Output, requireAudio bool) Output {
	out.ProbeFailures = nil
	out.Unplayable = false
	if out.VideoCodec == "" || out.Width == 0 || out.Height == 0 {
		out.Unplayable = true
		out.ProbeFailures = append(out.ProbeFailures, "no readable video stream")
	}
	if math.Abs(out.DurationSeconds-req.DurationSeconds) > 0.5 {
		out.ProbeFailures = append(out.ProbeFailures, fmt.Sprintf("duration %.2fs, requested %.0fs", out.DurationSeconds, req.DurationSeconds))
	}
	if out.Width > 0 && out.Height > 0 {
		w, h, _ := strings.Cut(req.AspectRatio, ":")
		rw, _ := strconv.ParseFloat(w, 64)
		rh, _ := strconv.ParseFloat(h, 64)
		if rh > 0 && math.Abs(float64(out.Width)/float64(out.Height)-rw/rh) > 0.02 {
			out.ProbeFailures = append(out.ProbeFailures, fmt.Sprintf("frame %dx%d is not %s", out.Width, out.Height, req.AspectRatio))
		}
	}
	if requireAudio && !out.HasAudio {
		out.ProbeFailures = append(out.ProbeFailures, "no audio stream")
	}
	return out
}
