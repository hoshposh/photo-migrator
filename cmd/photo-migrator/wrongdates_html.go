package main

import (
	"html/template"
	"io"
	"time"
)

// htmlDateLayout shows a date in the page's time zone with its offset, for
// example "29 Jul 2018, 2:17 PM (GMT-04:00)".
const htmlDateLayout = "2 Jan 2006, 3:04 PM (GMT-07:00)"

type wrongDatesPage struct {
	Generated string
	Rows      []wrongDatesPageRow
	NotJudged string
}

type wrongDatesPageRow struct {
	ID, File, Album, URL string
	SetTo, SetToUTC      string // the date to set, in the page's zone and in UTC
	GoogleHas            string
	Others               []string // dates other copies of the photo expect
}

// printWrongDatesHTML writes the report as one self-contained web page: a
// checklist whose ticks the browser remembers. Dates are shown in loc.
func printWrongDatesHTML(w io.Writer, rep dateReport, loc *time.Location, generated time.Time) error {
	page := wrongDatesPage{Generated: generated.In(loc).Format(htmlDateLayout), NotJudged: notJudgedText(rep)}
	for _, d := range rep.Wrong {
		r := wrongDatesPageRow{
			ID: d.MediaID, File: d.File, Album: d.Album, URL: d.URL,
			SetTo: d.ShouldBe.In(loc).Format(htmlDateLayout), SetToUTC: d.ShouldBe.UTC().Format(wrongDateTime),
			GoogleHas: d.GoogleHas.In(loc).Format(htmlDateLayout),
		}
		for _, t := range d.OtherExpected {
			r.Others = append(r.Others, t.In(loc).Format(htmlDateLayout))
		}
		page.Rows = append(page.Rows, r)
	}
	return wrongDatesTemplate.Execute(w, page)
}

var wrongDatesTemplate = template.Must(template.New("wrong-dates").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Wrong dates</title>
<style>
:root { color-scheme: light dark; --fg: #1f2328; --muted: #656d76; --bg: #fff; --line: #d0d7de; --accent: #0b57d0; --done: #f2f8f2; }
@media (prefers-color-scheme: dark) {
  :root { --fg: #e6edf3; --muted: #9198a1; --bg: #0d1117; --line: #30363d; --accent: #79b8ff; --done: #12261a; }
}
body { font: 15px/1.45 system-ui, sans-serif; color: var(--fg); background: var(--bg); margin: 0 auto; padding: 16px; max-width: 70rem; }
h1 { font-size: 1.4rem; margin: 0 0 .25rem; }
.muted { color: var(--muted); }
.counter { position: sticky; top: 0; background: var(--bg); padding: .5rem 0; font-weight: 600; border-bottom: 1px solid var(--line); }
table { border-collapse: collapse; width: 100%; margin-top: .5rem; }
th, td { text-align: left; vertical-align: top; padding: .5rem; border-bottom: 1px solid var(--line); }
th { color: var(--muted); font-weight: 600; font-size: .85rem; }
tr.done { background: var(--done); }
tr.done .file { text-decoration: line-through; color: var(--muted); }
.file { font-weight: 600; overflow-wrap: anywhere; }
.setto { font-weight: 600; }
a { color: var(--accent); }
@media (max-width: 40rem) { th { display: none; } td { display: block; border: 0; padding: .1rem .5rem; } tr { display: block; border-bottom: 1px solid var(--line); padding: .5rem 0; } }
</style>
</head>
<body>
<h1>Wrong dates</h1>
{{if .Rows -}}
<p class="muted">These photos were uploaded with a date that differs from the date taken. Google Photos cannot be told to change a date from outside, so fix each by hand: choose <b>Open in Google Photos</b>, then <b>&#8942; &rarr; Edit date &amp; time</b> (or the &#9432; panel), enter the date under <b>Set to</b>, and tick the box. Ticks are remembered in this browser.</p>
<div class="counter" id="counter">0 / {{len .Rows}} done</div>
<table>
<thead><tr><th>Done</th><th>Photo</th><th>Set to</th><th>Google has</th><th>Link</th></tr></thead>
<tbody>
{{range .Rows -}}
<tr>
<td><input type="checkbox" data-id="{{.ID}}" aria-label="Done: {{.File}}"></td>
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
{{if .NotJudged}}<p class="muted">Not judged: {{.NotJudged}}.</p>{{end}}
<p class="muted">Generated {{.Generated}}.</p>
{{if .Rows -}}
<script>
(function () {
  var KEY = 'photo-migrator-wrong-dates';
  var boxes = Array.prototype.slice.call(document.querySelectorAll('input[data-id]'));
  var counter = document.getElementById('counter');
  var saved = {};
  try { saved = JSON.parse(localStorage.getItem(KEY) || '{}') || {}; } catch (e) { saved = {}; }
  function refresh() {
    var n = 0;
    boxes.forEach(function (b) {
      if (b.checked) { n++; }
      b.closest('tr').classList.toggle('done', b.checked);
    });
    counter.textContent = n + ' / ' + boxes.length + ' done';
  }
  function save() {
    var ticked = {};
    boxes.forEach(function (b) { if (b.checked) { ticked[b.getAttribute('data-id')] = true; } });
    try { localStorage.setItem(KEY, JSON.stringify(ticked)); } catch (e) {}
  }
  boxes.forEach(function (b) {
    b.checked = saved[b.getAttribute('data-id')] === true;
    b.addEventListener('change', function () { refresh(); save(); });
  });
  refresh();
})();
</script>
{{end -}}
</body>
</html>
`))
