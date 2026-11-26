package main

import (
	"bytes"
	"strings"
	"testing"
)

// Break caught: the shared shell dropping the counter, the page's own storage
// key, the intro or the page's table rows, or loading anything from the
// network.
func TestChecklistTemplate_Shell(t *testing.T) {
	tmpl := newChecklistTemplate("test", `{{define "table"}}<table><tr><td><input type="checkbox" data-id="row-1">row one</td></tr></table>{{end}}`)
	var b bytes.Buffer
	err := tmpl.Execute(&b, checklistPage{
		Title: "Test page", Heading: "Test heading", Intro: "Do the thing.",
		StorageKey: "k-test", Generated: "today", Notes: []string{"a note"},
		Rows: []string{"one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"<title>Test page</title>", "<h1>Test heading</h1>", "Do the thing.",
		`id="counter">0 / 1 done`, "k-test", `data-id="row-1"`, "a note", "Generated today.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("page missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "http") {
		t.Errorf("page contains http:\n%s", out)
	}
}
