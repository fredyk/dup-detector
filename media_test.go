package main

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestParseFfprobeVideo(t *testing.T) {
	sample := []byte(`{
	  "streams": [
	    {"codec_type":"video","codec_name":"h264","width":1920,"height":1080,
	     "duration":"12.500000","avg_frame_rate":"30/1",
	     "tags":{"creation_time":"2021-05-05T10:00:00.000000Z"}}
	  ],
	  "format": {"duration":"12.500000","format_name":"mov,mp4,m4a",
	     "tags":{"creation_time":"2021-05-05T10:00:00.000000Z"}}
	}`)
	m := parseFfprobe(sample)
	if m == nil {
		t.Fatal("expected media metadata, got nil")
	}
	if m.Width != 1920 || m.Height != 1080 {
		t.Errorf("dims = %dx%d, want 1920x1080", m.Width, m.Height)
	}
	if m.DurationSec < 12.4 || m.DurationSec > 12.6 {
		t.Errorf("duration = %v, want ~12.5", m.DurationSec)
	}
	if m.Codec != "h264" {
		t.Errorf("codec = %q, want h264", m.Codec)
	}
	if m.CaptureTime != "2021-05-05T10:00:00.000000Z" {
		t.Errorf("capture time = %q", m.CaptureTime)
	}
}

func TestParseFfprobeEmptyIsNil(t *testing.T) {
	if m := parseFfprobe([]byte(`{"streams":[],"format":{}}`)); m != nil {
		t.Fatalf("empty ffprobe output should yield nil, got %+v", m)
	}
}

func TestImageDimensions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "img.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 7, 3))
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()

	w, h, ok := imageDimensions(p)
	if !ok || w != 7 || h != 3 {
		t.Fatalf("imageDimensions = %d,%d,%v; want 7,3,true", w, h, ok)
	}
}
