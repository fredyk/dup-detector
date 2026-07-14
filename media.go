package main

import (
	"encoding/json"
	"image"
	"os"
	"os/exec"
	"strconv"

	// Register the standard image decoders so image.DecodeConfig can read the
	// dimensions of these formats from just the header (no full decode).
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// MediaMetadata is optional multimedia info cached in the sidecar for images
// and videos, computed once (at copy time, while the bytes are read anyway) so
// later tools never re-parse or re-download the file to learn its dimensions,
// duration or capture date. Every field is omitempty — only what we could
// extract is stored.
type MediaMetadata struct {
	Width       int     `json:"width,omitempty"`
	Height      int     `json:"height,omitempty"`
	DurationSec float64 `json:"duration_sec,omitempty"`
	CaptureTime string  `json:"capture_time,omitempty"` // e.g. EXIF DateTimeOriginal / container creation_time
	Codec       string  `json:"codec,omitempty"`
	FrameRate   string  `json:"frame_rate,omitempty"`
	FormatName  string  `json:"format_name,omitempty"`
}

func (m *MediaMetadata) empty() bool {
	return m == nil || (m.Width == 0 && m.Height == 0 && m.DurationSec == 0 &&
		m.CaptureTime == "" && m.Codec == "" && m.FrameRate == "" && m.FormatName == "")
}

// extractMediaMetadata best-effort extracts multimedia metadata for path. It
// prefers ffprobe (a universal extractor for images AND videos) when present,
// and falls back to the standard-library image decoders for dimensions. Returns
// nil when nothing useful could be extracted (or the file isn't media). Never
// errors — media metadata is a bonus, never a reason to fail a copy.
func extractMediaMetadata(path string) *MediaMetadata {
	if m := ffprobeMetadata(path); !m.empty() {
		return m
	}
	if w, h, ok := imageDimensions(path); ok {
		return &MediaMetadata{Width: w, Height: h}
	}
	return nil
}

// ffprobeMetadata runs ffprobe (if installed) and parses its JSON. Returns nil
// when ffprobe is absent or produced nothing usable.
func ffprobeMetadata(path string) *MediaMetadata {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil
	}
	out, err := exec.Command(bin, "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", path).Output()
	if err != nil {
		return nil
	}
	return parseFfprobe(out)
}

// ffprobe JSON shape (only the fields we use).
type ffprobeOutput struct {
	Streams []struct {
		CodecType    string            `json:"codec_type"`
		CodecName    string            `json:"codec_name"`
		Width        int               `json:"width"`
		Height       int               `json:"height"`
		Duration     string            `json:"duration"`
		AvgFrameRate string            `json:"avg_frame_rate"`
		Tags         map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		Duration   string            `json:"duration"`
		FormatName string            `json:"format_name"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

// parseFfprobe turns ffprobe's JSON into a MediaMetadata, picking the first
// image/video stream for dimensions/codec and the container/stream tags for the
// capture time. Returns nil when nothing meaningful is present.
func parseFfprobe(data []byte) *MediaMetadata {
	var out ffprobeOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	m := &MediaMetadata{FormatName: out.Format.FormatName}
	for _, s := range out.Streams {
		if s.CodecType == "video" || s.CodecType == "image" {
			if m.Width == 0 && s.Width > 0 {
				m.Width, m.Height = s.Width, s.Height
				m.Codec = s.CodecName
				if s.AvgFrameRate != "" && s.AvgFrameRate != "0/0" {
					m.FrameRate = s.AvgFrameRate
				}
			}
			if m.CaptureTime == "" {
				m.CaptureTime = firstTag(s.Tags, "creation_time", "DateTimeOriginal", "com.apple.quicktime.creationdate")
			}
		}
	}
	if d := parseDuration(out.Format.Duration); d > 0 {
		m.DurationSec = d
	} else {
		for _, s := range out.Streams {
			if d := parseDuration(s.Duration); d > 0 {
				m.DurationSec = d
				break
			}
		}
	}
	if m.CaptureTime == "" {
		m.CaptureTime = firstTag(out.Format.Tags, "creation_time", "DateTimeOriginal", "com.apple.quicktime.creationdate")
	}
	if m.empty() {
		return nil
	}
	return m
}

func firstTag(tags map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := tags[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

func parseDuration(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// imageDimensions reads only the header of a standard image format (jpeg, png,
// gif) to get its dimensions, without decoding the pixels. ok=false when the
// file isn't a decodable image.
func imageDimensions(path string) (w, h int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}
