package main

import (
	"io"
	"strconv"
	"time"
)

// htmlDateLayout shows a date in the page's time zone with its offset, for
// example "29 Jul 2018, 2:17 PM (GMT-04:00)".
const htmlDateLayout = "2 Jan 2006, 3:04 PM (GMT-07:00)"

type wrongDatesPageRow struct {
	ID, File, Album, URL string
	Key                  string // media ID and when Google's date was read: a recheck changes it, clearing the tick
	SetTo, SetToUTC      string // the date to set, in the page's zone and in UTC
	GoogleHas            string
	Others               []string // dates other copies of the photo expect
}

// printWrongDatesHTML writes the report as one self-contained web page: a
// checklist whose ticks the browser remembers. Dates are shown in loc.
func printWrongDatesHTML(w io.Writer, rep dateReport, loc *time.Location, generated time.Time) error {
	var rows []wrongDatesPageRow
	for _, d := range rep.Wrong {
		r := wrongDatesPageRow{
			ID: d.MediaID, Key: d.MediaID + "@" + strconv.FormatInt(max(d.GoogleCheckedAt.Unix(), 0), 10), File: d.File, Album: d.Album, URL: d.URL,
			SetTo: d.ShouldBe.In(loc).Format(htmlDateLayout), SetToUTC: d.ShouldBe.UTC().Format(wrongDateTime),
			GoogleHas: d.GoogleHas.In(loc).Format(htmlDateLayout),
		}
		for _, t := range d.OtherExpected {
			r.Others = append(r.Others, t.In(loc).Format(htmlDateLayout))
		}
		rows = append(rows, r)
	}
	page := checklistPage{
		Title: "Wrong dates", Heading: "Wrong dates",
		Intro:      wrongDatesIntro,
		StorageKey: wrongDatesStorageKey,
		Generated:  generated.In(loc).Format(htmlDateLayout),
		Rows:       rows,
	}
	if nj := notJudgedText(rep); nj != "" {
		page.Notes = []string{"Not judged: " + nj + "."}
	}
	return wrongDatesTemplate.Execute(w, page)
}

// wrongDatesStorageKey is where the browser keeps this page's ticks. Keep it
// unchanged so ticks made on earlier pages still show.
const wrongDatesStorageKey = "photo-migrator-wrong-dates"

const wrongDatesIntro = `These photos were uploaded with a date that differs from the date taken. Google Photos cannot be told to change a date from outside, so fix each by hand: choose <b>Open in Google Photos</b>, then <b>&#8942; &rarr; Edit date &amp; time</b> (or the &#9432; panel), enter the date under <b>Set to</b>, and tick the box. Ticks are remembered in this browser.`

var wrongDatesTemplate = newChecklistTemplate("wrong-dates", `{{define "table" -}}
{{if . -}}
<table>
<thead><tr><th>Done</th><th>Photo</th><th>Set to</th><th>Google has</th><th>Link</th></tr></thead>
<tbody>
{{range . -}}
<tr>
<td><input type="checkbox" data-id="{{.Key}}" aria-label="Done: {{.File}}"></td>
<td><span class="file">{{.File}}</span>{{if .Album}}<br><span class="muted">{{.Album}}</span>{{end}}</td>
<td><span class="setto">{{.SetTo}}</span><br><span class="muted">{{.SetToUTC}}</span>{{range .Others}}<br><span class="muted">another copy expects {{.}}</span>{{end}}</td>
<td>{{.GoogleHas}}</td>
<td>{{if .URL}}<a href="{{.URL}}" target="_blank" rel="noopener">Open in Google Photos</a>{{else}}<span class="muted">no link</span>{{end}}</td>
</tr>
{{end -}}
</tbody>
</table>
{{else -}}
<p>No uploaded photos have a wrong date.</p>
{{end -}}
{{end}}`)
