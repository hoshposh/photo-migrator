package takeout

import (
	"fmt"
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

// takeoutNameGroup is one folder of a Takeout export: its sidecar files (name
// -> title) and, per media file, the sidecar it should get ("" = none). Each
// sidecar's description is its own file name, so a lookup that returns only
// the parsed sidecar can still say which file it found.
type takeoutNameGroup struct {
	name     string
	sidecars map[string]string // sidecar file name -> title
	want     map[string]string // media file name -> expected sidecar ("" = none)
}

// takeoutNameCases are real names from a Google Takeout export that the
// original four strategies missed (or got wrong).
var takeoutNameCases = []takeoutNameGroup{
	{
		name: "numbered duplicate",
		sidecars: map[string]string{
			"X.jpg.supplemental-metada.json":                "X.jpg",
			"X.jpg.supplemental-metada(1).json":             "X.jpg",
			"image000000.jpg.supplemental-metadata(2).json": "image000000.jpg",
			"short.jpg.supplemental-metadata.json":          "short.jpg",
		},
		want: map[string]string{
			// Break caught: X.jpg taking its duplicate's sidecar, because
			// "…metada(1).json" sorts before "…metada.json".
			"X.jpg":              "X.jpg.supplemental-metada.json",
			"X(1).jpg":           "X.jpg.supplemental-metada(1).json",
			"image000000(2).jpg": "image000000.jpg.supplemental-metadata(2).json",
			// Break caught: a duplicate falling back to the original's sidecar.
			"short(1).jpg": "",
		},
	},
	{
		name: "own name ends in (N)",
		sidecars: map[string]string{
			"Screenshot_20200101-120000(1).png.supplemental-me.json":    "Screenshot_20200101-120000(1).png",
			"Screenshot_20200101-120000(1).png.supplemental-me(1).json": "Screenshot_20200101-120000(1).png",
			"Y(1).png.supplemental-me(1).json":                          "Y(1).png",
		},
		want: map[string]string{
			// Break caught: a file whose own name ends in "(1)" taking the
			// sidecar of its duplicate "…(1)(1).png".
			"Screenshot_20200101-120000(1).png":    "Screenshot_20200101-120000(1).png.supplemental-me.json",
			"Screenshot_20200101-120000(1)(1).png": "Screenshot_20200101-120000(1).png.supplemental-me(1).json",
			// Its own sidecar is elsewhere: still never the duplicate's.
			"Y(1).png": "",
		},
	},
	{
		name: "edited copy",
		sidecars: map[string]string{
			"IMG_20141108_150533.jpg.supplemental-metadata.json": "IMG_20141108_150533.jpg",
			"IMG_0359-EFFECTS.jpg.supplemental-metadata.json":    "IMG_0359-EFFECTS.jpg",
			"DSC_0180.JPG.supplemental-metadata.json":            "DSC_0180.JPG",
			"DSC_0180.JPG.supplemental-metadata(1).json":         "DSC_0180.JPG",
		},
		want: map[string]string{
			"IMG_20141108_150533-edited.jpg": "IMG_20141108_150533.jpg.supplemental-metadata.json",
			"IMG_0359-EFFECTS-edited.jpg":    "IMG_0359-EFFECTS.jpg.supplemental-metadata.json",
			"DSC_0180-edited(1).JPG":         "DSC_0180.JPG.supplemental-metadata(1).json",
		},
	},
	{
		name: "live photo video",
		sidecars: map[string]string{
			"IMG_7297.HEIC.supplemental-metadata.json":             "IMG_7297.HEIC",
			"MVIMG_20190901_150451.jpg.supplemental-metadata.json": "MVIMG_20190901_150451.jpg",
			"IMG_1.HEIC.supplemental-metadata.json":                "IMG_1.HEIC",
			"IMG_1.JPG.supplemental-metadata.json":                 "IMG_1.JPG",
		},
		want: map[string]string{
			"IMG_7297.MP4":              "IMG_7297.HEIC.supplemental-metadata.json",
			"MVIMG_20190901_150451.MP4": "MVIMG_20190901_150451.jpg.supplemental-metadata.json",
			// Two images share the stem: ambiguous, so no match.
			"IMG_1.MP4": "",
		},
	},
	{
		name: "name cut short",
		sidecars: map[string]string{
			"00100dPORTRAIT_00100_BURST20191129125653882_CO.json":    "00100dPORTRAIT_00100_BURST20191129125653882_COVER.jpg",
			"PXL_20220924_162743977_exported_1397_168760711.json":    "PXL_20220924_162743977_exported_1397_1687607112098.jpg",
			"PXL_20220924_162743977_exported_1397_168760711(1).json": "PXL_20220924_162743977_exported_1397_1687607112098~2.jpg",
			"IMG_12.json": "IMG_1234.jpeg",
		},
		want: map[string]string{
			"00100dPORTRAIT_00100_BURST20191129125653882_COV.jpg":    "00100dPORTRAIT_00100_BURST20191129125653882_CO.json",
			"PXL_20220924_162743977_exported_1397_1687607112.jpg":    "PXL_20220924_162743977_exported_1397_168760711.json",
			"PXL_20220924_162743977_exported_1397_1687607112(1).jpg": "PXL_20220924_162743977_exported_1397_168760711(1).json",
			// Break caught: a different photo sharing the long prefix taking
			// the cut-short sidecar (its title names ...1687607112098).
			"PXL_20220924_162743977_exported_1397_1687607113.jpg": "",
			// Below the 30-character minimum.
			"IMG_1234.jpg": "",
		},
	},
}

func takeoutNameJSON(name, title string) string {
	return fmt.Sprintf(`{"title":%q,"description":%q,"photoTakenTime":{"timestamp":"1560535320"}}`, title, name)
}

func TestFindSidecar_TakeoutNames(t *testing.T) {
	for _, g := range takeoutNameCases {
		t.Run(g.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, title := range g.sidecars {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(takeoutNameJSON(name, title)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for media := range g.want {
				if err := os.WriteFile(filepath.Join(dir, media), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for media, wantFile := range g.want {
				path, _, err := FindSidecar(filepath.Join(dir, media))
				if err != nil {
					t.Fatalf("%s: %v", media, err)
				}
				if got := filepath.Base(path); path == "" && wantFile != "" || path != "" && got != wantFile {
					t.Errorf("FindSidecar(%s) = %q; want %q", media, path, wantFile)
				}
			}
		})
	}
}
