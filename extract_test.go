package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestNextDataMD(t *testing.T) {
	raw := `{"props":{"pageProps":{"article":{"title":"Test Page","intro":"This is a long enough prose string that should be included because it contains spaces and is over 80 chars long.","buildId":"skip"}}},"page":"/test","buildId":"abc123"}`
	got := nextDataMD(raw)
	if got == "" {
		t.Fatal("expected non-empty result from __NEXT_DATA__")
	}
	if !strings.Contains(got, "This is a long enough prose") {
		t.Errorf("expected prose content, got: %q", got)
	}
}

func TestLDJsonMD(t *testing.T) {
	raw := `{"@type":"Article","headline":"My Article","description":"A description that is long enough to qualify as prose content."}`
	got := ldJSONMD(raw)
	if !strings.Contains(got, "My Article") {
		t.Errorf("expected headline, got: %q", got)
	}
}

func TestIsProseString(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"short", false},
		{"https://example.com/path/to/something", false},
		{"/path/to/resource", false},
		{"<div class='foo'>bar</div>", false},
		{strings.Repeat("a", 100), false}, // no spaces, < 300 chars
		{"This is a long enough sentence that contains multiple words and spaces and should be considered prose content.", true},
		{"data:image/png;base64,abc123...", false},
	}
	for _, c := range cases {
		if got := isProseString(c.s); got != c.want {
			t.Errorf("isProseString(%q) = %v, want %v", c.s[:min(20, len(c.s))], got, c.want)
		}
	}
}

func min(a, b int) int {
	if a < b { return a }
	return b
}

func TestRenderMarkdownTable(t *testing.T) {
	headers := []string{"Name", "Type", "Description"}
	rows := [][]string{
		{"ABAN8", "MATH", "Address Number"},
		{"ABALPH", "CHAR", "Alpha Name"},
	}
	got := renderMarkdownTable(headers, rows)
	if !strings.Contains(got, "| Name | Type | Description |") {
		t.Errorf("missing header row, got:\n%s", got)
	}
	if !strings.Contains(got, "ABAN8") || !strings.Contains(got, "Address Number") {
		t.Errorf("missing data, got:\n%s", got)
	}
}

func TestScanScriptForDT(t *testing.T) {
	script := `
		let table = new DataTable('#myTable', {
			ajax: '/api/data/columns',
			columns: [{title:'Name'},{title:'Type'}]
		});`
	base, _ := url.Parse("https://example.com")
	sel, ajaxURL := scanScriptForDT(script, base)
	if sel != "#myTable" {
		t.Errorf("selector = %q, want %q", sel, "#myTable")
	}
	if ajaxURL != "https://example.com/api/data/columns" {
		t.Errorf("ajaxURL = %q, want https://example.com/api/data/columns", ajaxURL)
	}
}

func TestParseDataTablesResponse(t *testing.T) {
	resp := map[string]interface{}{
		"data": []interface{}{
			[]interface{}{"ABAN8", "MATH", "Address Number"},
			[]interface{}{"ABALPH", "CHAR", "Alpha Name"},
		},
	}
	rows, _ := parseDataTablesResponse(resp)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0][0] != "ABAN8" {
		t.Errorf("row[0][0] = %q, want ABAN8", rows[0][0])
	}
}

func TestTableBodyHasRows(t *testing.T) {
	emptyTable := []byte(`<html><body><table id="t1"><thead><tr><th>A</th></tr></thead><tbody></tbody></table></body></html>`)
	if tableBodyHasRows(emptyTable, "#t1") {
		t.Error("expected false for empty tbody")
	}
	
	fullTable := []byte(`<html><body><table id="t2"><thead><tr><th>A</th></tr></thead><tbody><tr><td>x</td></tr></tbody></table></body></html>`)
	if !tableBodyHasRows(fullTable, "#t2") {
		t.Error("expected true for tbody with rows")
	}
}
