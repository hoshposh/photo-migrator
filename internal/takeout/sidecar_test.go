package takeout

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSidecarTakenTime(t *testing.T) {
	want := time.Date(2019, 6, 14, 18, 2, 0, 0, time.UTC)
	ts := func(s string) EpochTime { return EpochTime{Timestamp: s} }
	tests := []struct {
		name   string
		s      *Sidecar
		want   time.Time
		wantOK bool
	}{
		{"photoTakenTime", &Sidecar{PhotoTakenTime: ts("1560535320")}, want, true},
		{"zero photoTakenTime falls back to creationTime", &Sidecar{PhotoTakenTime: ts("0"), CreationTime: ts("1560535320")}, want, true},
		{"empty photoTakenTime falls back to creationTime", &Sidecar{CreationTime: ts("1560535320")}, want, true},
		{"both empty", &Sidecar{}, time.Time{}, false},
		{"not a number", &Sidecar{PhotoTakenTime: ts("abc")}, time.Time{}, false},
		{"negative", &Sidecar{PhotoTakenTime: ts("-5")}, time.Time{}, false},
		{"nil sidecar", nil, time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.s.TakenTime()
			if ok != tt.wantOK || !got.Equal(tt.want) {
				t.Errorf("TakenTime() = %v, %v; want %v, %v", got, ok, tt.want, tt.wantOK)
			}
			if ok && got.Location() != time.UTC {
				t.Errorf("TakenTime() location = %v, want UTC", got.Location())
			}
		})
	}
}

func TestFindSidecar_Strategies(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.jpg.supplemental-metadata.json", `{"title":"a.jpg"}`)
	write("b.jpg.json", `{"title":"b.jpg"}`)
	write("long-name.jpg.supplemental-me.json", `{"title":"long-name.jpg"}`)
	write("x.json", `{"title":"c.jpg"}`)
	write("metadata.json", `{"title":"d.jpg"}`)

	for media, wantFile := range map[string]string{
		"a.jpg":         "a.jpg.supplemental-metadata.json",
		"b.jpg":         "b.jpg.json",
		"long-name.jpg": "long-name.jpg.supplemental-me.json",
		"c.jpg":         "x.json",
		"d.jpg":         "", // metadata.json is not a sidecar
		"nothing.jpg":   "",
	} {
		path, s, err := FindSidecar(filepath.Join(dir, media))
		if err != nil {
			t.Fatalf("%s: %v", media, err)
		}
		want := ""
		if wantFile != "" {
			want = filepath.Join(dir, wantFile)
		}
		if path != want || (want == "") != (s == nil) {
			t.Errorf("FindSidecar(%s) = %q, %v; want %q", media, path, s, want)
		}
	}
}
