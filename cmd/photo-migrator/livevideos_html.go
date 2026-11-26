package main

import (
	"io"
	"time"
)

type liveVideoPageRow struct {
	ID, File, Album, URL, GoogleDate string
}

// liveVideosStorageKey is where the browser keeps this page's ticks.
const liveVideosStorageKey = "photo-migrator-live-videos"

const liveVideosIntro = `Open each video and delete it (&#128465;). The still photo is a separate item and stays.`

// printUploadedVideosHTML writes the report as a self-contained checklist
// page, with dates shown in loc.
func printUploadedVideosHTML(w io.Writer, rep uploadedVideosReport, loc *time.Location, generated time.Time) error {
	var rows []liveVideoPageRow
	for _, v := range rep.Videos {
		r := liveVideoPageRow{ID: v.MediaID, File: v.File, Album: v.Album, URL: v.URL}
		if !v.GoogleTime.IsZero() {
			r.GoogleDate = v.GoogleTime.In(loc).Format(htmlDateLayout)
		}
		rows = append(rows, r)
	}
	page := checklistPage{
		Title: "Live Photo videos", Heading: "Live Photo videos",
		Intro:      liveVideosIntro,
		StorageKey: liveVideosStorageKey,
		Generated:  generated.In(loc).Format(htmlDateLayout),
		Rows:       rows,
	}
	if c := rep.countsText(); c != "" {
		page.Notes = []string{c}
	}
	return liveVideosTemplate.Execute(w, page)
}

var liveVideosTemplate = newChecklistTemplate("live-videos", `{{define "table" -}}
{{if . -}}
<table>
<thead><tr><th>Done</th><th>Video</th><th>Album</th><th>Google's date</th><th>Link</th></tr></thead>
<tbody>
{{range . -}}
<tr>
<td><input type="checkbox" data-id="{{.ID}}" aria-label="Done: {{.File}}"></td>
<td><span class="file">{{.File}}</span></td>
<td>{{if .Album}}{{.Album}}{{else}}<span class="muted">-</span>{{end}}</td>
<td>{{if .GoogleDate}}{{.GoogleDate}}{{else}}<span class="muted">-</span>{{end}}</td>
<td>{{if .URL}}<a href="{{.URL}}" target="_blank" rel="noopener">Open in Google Photos</a>{{else}}<span class="muted">no link</span>{{end}}</td>
</tr>
{{end -}}
</tbody>
</table>
{{else -}}
<p>No Live Photo videos to remove.</p>
{{end -}}
{{end}}`)
