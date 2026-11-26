package takeout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLivePairs(t *testing.T) {
	const f = "Takeout/Google Photos/Trip/"
	tests := []struct {
		name  string
		add   []string
		video string
		want  bool
	}{
		{"heic + mp4", []string{f + "IMG_7297.HEIC"}, f + "IMG_7297.MP4", true},
		{"jpg + mp4", []string{f + "MVIMG_1.jpg"}, f + "MVIMG_1.mp4", true},
		{"jpeg + mov", []string{f + "IMG_2.JPEG"}, f + "IMG_2.MOV", true},
		{"different folder", []string{"Takeout/Google Photos/Other/IMG_3.heic"}, f + "IMG_3.mov", false},
		{"png is not a still", []string{f + "clip.png"}, f + "clip.mov", false},
		{"video alone", nil, f + "IMG_4.MP4", false},
		{"json is not a still or video", []string{f + "IMG_6.jpg"}, f + "IMG_6.MP4.json", false},
		{"stems compared exactly", []string{f + "IMG_7.HEIC"}, f + "img_7.MP4", false},
		{"not a video", []string{f + "IMG_8.HEIC"}, f + "IMG_8.HEIC", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewLivePairs()
			for _, e := range tt.add {
				p.Add(e)
			}
			if got := p.IsVideoHalf(tt.video); got != tt.want {
				t.Errorf("IsVideoHalf(%q) = %v, want %v", tt.video, got, tt.want)
			}
		})
	}
	t.Run("nil receiver", func(t *testing.T) {
		var p *LivePairs
		if p.IsVideoHalf(f + "IMG_1.MP4") {
			t.Error("nil LivePairs reported a video half")
		}
	})
}

func TestLivePairs_AddZips(t *testing.T) {
	dir := t.TempDir()
	z1, z2 := filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.zip")
	bad := filepath.Join(dir, "bad.zip")
	writeZip(t, z1, "Takeout/Google Photos/Trip/IMG_1.HEIC")
	writeZip(t, z2, "Takeout/Google Photos/Trip/IMG_1.MP4")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := NewLivePairs()
	failed, err := p.AddZips(context.Background(), []string{z1, bad, z2})
	if err != nil {
		t.Fatalf("AddZips: %v", err)
	}
	if len(failed) != 1 || failed[0] != bad {
		t.Errorf("failed = %v, want [%s]", failed, bad)
	}
	if !p.IsVideoHalf("Takeout/Google Photos/Trip/IMG_1.MP4") {
		t.Error("video in zip 2 not recognised by still in zip 1")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewLivePairs().AddZips(ctx, []string{z1, z2}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled AddZips err = %v, want context.Canceled", err)
	}
}

func TestLivePairs_AddDir(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "Takeout", "Google Photos", "Trip")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"IMG_1.HEIC", "IMG_1.MP4"} {
		if err := os.WriteFile(filepath.Join(folder, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := NewLivePairs()
	if err := p.AddDir(dir); err != nil {
		t.Fatalf("AddDir: %v", err)
	}
	if !p.IsVideoHalf("Takeout/Google Photos/Trip/IMG_1.MP4") {
		t.Error("IMG_1.MP4 not recognised as a video half")
	}
}
