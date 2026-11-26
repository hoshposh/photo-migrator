package main

import "html/template"

// checklistPage is what the shared shell renders around a page's own table.
type checklistPage struct {
	Title, Heading string
	// Intro is one paragraph of instructions; it may use <b> and entities. It is
	// trusted static markup only: never put file or album names or other data in
	// it, because they would not be escaped.
	Intro      template.HTML
	StorageKey string // localStorage key for the ticks
	Generated  string
	Notes      []string // lines under the table (counts, hints)
	Rows       any      // passed to the page's "table" template
}

// newChecklistTemplate returns the shared shell (CSS, counter, tick JS) with
// the page's table definition parsed in as {{define "table"}}…{{end}}. The
// table's checkboxes must be <input type="checkbox" data-id="KEY">. The table
// template runs whether or not there are rows, so it also writes the page's
// "nothing to do" message; the intro, counter and script appear only when
// Rows is not empty.
func newChecklistTemplate(name, table string) *template.Template {
	return template.Must(template.Must(template.New(name).Parse(checklistShell)).Parse(table))
}

const checklistShell = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
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
<h1>{{.Heading}}</h1>
{{if .Rows -}}
<p class="muted">{{.Intro}}</p>
<div class="counter" id="counter">0 / {{len .Rows}} done</div>
{{end -}}
{{template "table" .Rows}}
{{range .Notes}}<p class="muted">{{.}}</p>
{{end -}}
<p class="muted">Generated {{.Generated}}.</p>
{{if .Rows -}}
<script>
(function () {
  var KEY = {{.StorageKey}};
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
`
