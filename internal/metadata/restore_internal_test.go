package metadata

import (
	"errors"
	"testing"
)

// Break caught: the one-line summary keeping exiftool's first line, a
// harmless "[minor] ... Fixed." warning, and hiding the error that actually
// stopped the write (seen on Samsung Galaxy S23 photos).
func TestFirstLine_PrefersTheErrorOverWarnings(t *testing.T) {
	const path = "/staging/takeout-002/Takeout/Google Photos/Photos from 2023/20231005_141446.jpg"
	cases := []struct {
		name, output, want string
	}{
		{
			"warning before error",
			"Warning: [minor] Entries in IFD0 were out of sequence. Fixed. - " + path + "\n" +
				"Error: Error reading OtherImageStart data in IFD0 - " + path + "\n" +
				"    0 image files updated\n    1 files weren't updated due to errors\n",
			"Error: Error reading OtherImageStart data in IFD0",
		},
		{
			"error only",
			"Error: Not a valid JPG (looks more like a TXT) - " + path + "\n    0 image files updated\n",
			"Error: Not a valid JPG (looks more like a TXT)",
		},
		{
			"warning only",
			"Warning: Something odd - " + path + "\n",
			"Warning: Something odd",
		},
		{"no output", "", "exit status 1"},
	}
	for _, c := range cases {
		if got := firstLine([]byte(c.output), path, errors.New("exit status 1")); got != c.want {
			t.Errorf("%s: firstLine = %q, want %q", c.name, got, c.want)
		}
	}
}
