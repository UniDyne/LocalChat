package main

// Dynamic-content extraction for web_fetch — two tiers:
//
//   Tier 1 — Inline JSON state: frameworks like Next.js embed page data in
//   <script> tags as JSON. Readability ignores these. When the extracted
//   content is thin (< 150 words), we scan for __NEXT_DATA__, ld+json, and
//   window.__INITIAL_STATE__ blobs and convert any prose strings to markdown.
//
//   Tier 2 — DataTables AJAX: many reference/data pages render an empty
//   <table> in the HTML and populate it via a DataTable({ajax:'url'}) call.
//   When the table body is empty and an AJAX URL is discoverable in inline
//   scripts, we fetch that URL and render the response as a markdown table.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ---------------------------------------------------------------------------
// Entry points called from fetchPage
// ---------------------------------------------------------------------------

// inlineStateMD scans body for known inline JSON state patterns and returns
// any discovered prose content as Markdown. Returns "" when nothing useful
// is found.
func inlineStateMD(body []byte) string {
	inline, ldScripts, nextDataScript := partitionScripts(body)

	var parts []string

	if s := nextDataMD(nextDataScript); s != "" {
		parts = append(parts, s)
	}
	if s := ldJSONMD(ldScripts); s != "" {
		parts = append(parts, s)
	}
	if s := windowStateMD(inline); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

// dataTablesAjaxMD scans body for a DataTables initialisation with a
// discoverable AJAX URL whose table body is empty in the static HTML.
// When found, fetches the endpoint and returns a markdown table. Returns "".
func dataTablesAjaxMD(body []byte, base *url.URL) string {
	selector, ajaxURL := findDataTablesAjax(body, base)
	if ajaxURL == "" {
		return ""
	}
	// Skip if the table already has static rows — no need to re-fetch.
	if tableBodyHasRows(body, selector) {
		return ""
	}
	headers := findTableHeaders(body, selector)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", ajaxURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "application/json, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", base.String())

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return ""
	}

	var dtResp map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&dtResp); err != nil {
		return ""
	}

	rows, colNames := parseDataTablesResponse(dtResp)
	if len(rows) == 0 {
		return ""
	}
	if len(headers) == 0 {
		headers = colNames
	}
	return renderMarkdownTable(headers, rows)
}

// wordCount returns the number of whitespace-separated tokens in s.
func wordCount(s string) int { return len(strings.Fields(s)) }

// ---------------------------------------------------------------------------
// Script partitioning (shared by Tier 1 helpers)
// ---------------------------------------------------------------------------

// partitionScripts walks the HTML parse tree and separates script nodes into:
//   - inline: concatenated text of inline scripts (no src=)
//   - ldScripts: concatenated text of type="application/ld+json" scripts
//   - nextDataScript: text of id="__NEXT_DATA__" script (first match)
func partitionScripts(body []byte) (inline, ldScripts, nextDataScript string) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "script" {
			attrs := nodeAttrs(n)
			text := nodeInnerText(n)
			switch {
			case attrs["id"] == "__NEXT_DATA__":
				nextDataScript = text
			case strings.Contains(attrs["type"], "ld+json"):
				ldScripts += text + "\n"
			case attrs["src"] == "":
				inline += text + "\n"
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return
}

// nodeAttrs returns the attribute map of an HTML element node.
func nodeAttrs(n *html.Node) map[string]string {
	m := make(map[string]string, len(n.Attr))
	for _, a := range n.Attr {
		m[a.Key] = a.Val
	}
	return m
}

// nodeInnerText returns the concatenated text content of a node's children.
func nodeInnerText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------------------
// Tier 1: Inline JSON state
// ---------------------------------------------------------------------------

// nextDataMD extracts prose content from a raw __NEXT_DATA__ JSON string.
func nextDataMD(raw string) string {
	if raw == "" {
		return ""
	}
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return ""
	}
	props, _ := root["props"].(map[string]interface{})
	if props == nil {
		return ""
	}
	pageProps, _ := props["pageProps"].(map[string]interface{})
	if pageProps == nil {
		return ""
	}
	var strs []string
	collectProseStrings(pageProps, 0, &strs)
	return strings.Join(strs, "\n\n")
}

// ldJSONMD converts one or more application/ld+json blobs to markdown.
func ldJSONMD(raw string) string {
	if raw == "" {
		return ""
	}
	var parts []string
	dec := json.NewDecoder(strings.NewReader(raw))
	for dec.More() {
		var obj map[string]interface{}
		if err := dec.Decode(&obj); err != nil {
			break
		}
		for _, key := range []string{"headline", "name", "description", "articleBody", "text"} {
			if v, ok := obj[key].(string); ok && len(v) > 30 {
				parts = append(parts, v)
				break
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// windowStateME searches inline script text for window.__INITIAL_STATE__,
// window.__PRELOADED_STATE__, window.__data, or window.__NUXT__ assignments
// and returns extracted prose content.
var windowStateRE = regexp.MustCompile(
	`(?i)window\.(?:__INITIAL_STATE__|__PRELOADED_STATE__|__data|__NUXT__)\s*=\s*`)

func windowStateMD(inline string) string {
	for _, loc := range windowStateRE.FindAllStringIndex(inline, -1) {
		// Scan past whitespace to the opening brace.
		rest := inline[loc[1]:]
		start := strings.IndexByte(rest, '{')
		if start < 0 {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(rest[start:]))
		var obj map[string]interface{}
		if err := dec.Decode(&obj); err != nil {
			continue
		}
		var strs []string
		collectProseStrings(obj, 0, &strs)
		if len(strs) > 0 {
			return strings.Join(strs, "\n\n")
		}
	}
	return ""
}

// noisyJSONKeys are framework/routing keys that never contain page content.
var noisyJSONKeys = map[string]bool{
	"buildId": true, "locale": true, "locales": true, "defaultLocale": true,
	"isPreview": true, "isFallback": true, "gssp": true, "gip": true, "appGip": true,
	"runtimeConfig": true, "nextExport": true, "autoExport": true, "assetPrefix": true,
	"scriptLoader": true, "err": true, "dynamicIds": true, "customServer": true,
	"query": true, "page": true, "pathname": true, "asPath": true,
	"__N_SSP": true, "__N_SSG": true,
}

// collectProseStrings recursively walks a JSON value and appends string
// values that look like human-readable prose to out.
func collectProseStrings(v interface{}, depth int, out *[]string) {
	if depth > 6 {
		return
	}
	switch val := v.(type) {
	case string:
		if isProseString(val) {
			*out = append(*out, strings.TrimSpace(val))
		}
	case map[string]interface{}:
		for k, child := range val {
			if !noisyJSONKeys[k] {
				collectProseStrings(child, depth+1, out)
			}
		}
	case []interface{}:
		limit := len(val)
		if limit > 20 {
			limit = 20
		}
		for _, item := range val[:limit] {
			collectProseStrings(item, depth+1, out)
		}
	}
}

// isProseString returns true when s looks like human-readable prose rather
// than a URL, hash, config value, or code blob.
func isProseString(s string) bool {
	if len(s) < 80 {
		return false
	}
	// URL or path
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "/") {
		return false
	}
	// JSON, HTML, or code
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") ||
		strings.HasPrefix(s, "<") {
		return false
	}
	// Data URI or base64 blob
	if strings.Contains(s, "data:") || strings.Contains(s, "base64,") {
		return false
	}
	// Hash / token: long but no spaces
	if len(s) < 300 && !strings.Contains(s, " ") {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Tier 2: DataTables AJAX
// ---------------------------------------------------------------------------

var (
	// DataTable('selector', {  or  new DataTable('selector', {
	reDTSelector = regexp.MustCompile(`(?i)DataTable\s*\(\s*['"]([^'"]+)['"]`)
	// ajax: 'url'
	reDTAjaxStr = regexp.MustCompile(`(?i)\bajax\s*:\s*['"]([^'"#][^'"]{0,500})['"]`)
	// url: 'url'  (inside an ajax:{} block)
	reDTURLKey = regexp.MustCompile(`(?i)\burl\s*:\s*['"]([^'"#][^'"]{0,500})['"]`)
)

// findDataTablesAjax scans inline scripts for a DataTable() call that
// includes an ajax option with a string URL. Returns the CSS selector string
// and the fully resolved AJAX URL, or empty strings if not found.
func findDataTablesAjax(body []byte, base *url.URL) (selector, ajaxURL string) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if ajaxURL != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "script" {
			attrs := nodeAttrs(n)
			if attrs["src"] == "" {
				if sel, u := scanScriptForDT(nodeInnerText(n), base); u != "" {
					selector, ajaxURL = sel, u
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return
}

// scanScriptForDT scans a single inline script for DataTable AJAX patterns.
func scanScriptForDT(script string, base *url.URL) (selector, ajaxURL string) {
	for _, loc := range reDTSelector.FindAllStringSubmatchIndex(script, -1) {
		sel := script[loc[2]:loc[3]]

		// Search within the next 2000 chars for the ajax option.
		end := loc[1] + 2000
		if end > len(script) {
			end = len(script)
		}
		block := script[loc[1]:end]

		var rawURL string
		if m := reDTAjaxStr.FindStringSubmatch(block); m != nil {
			rawURL = m[1]
		} else if m := reDTURLKey.FindStringSubmatch(block); m != nil {
			rawURL = m[1]
		}
		if rawURL == "" {
			continue
		}

		parsed, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			continue
		}
		resolved := base.ResolveReference(parsed)
		if resolved.Scheme != "http" && resolved.Scheme != "https" {
			continue
		}
		return sel, resolved.String()
	}
	return "", ""
}

// tableBodyHasRows returns true when the table identified by selector
// already contains <tr> elements in its <tbody>.
func tableBodyHasRows(body []byte, selector string) bool {
	tableID := strings.TrimPrefix(strings.TrimPrefix(selector, "#"), ".")
	if tableID == "" {
		return false
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return false
	}
	var found, hasRows bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && n.Data == "table" {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == tableID {
					found = true
					hasRows = tbodyHasRows(n)
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return hasRows
}

func tbodyHasRows(tableNode *html.Node) bool {
	var check func(*html.Node) bool
	check = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "tbody" {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "tr" {
					return true
				}
			}
			return false
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if check(c) {
				return true
			}
		}
		return false
	}
	return check(tableNode)
}

// findTableHeaders locates the table identified by selector and returns the
// text of its <thead> header cells.
func findTableHeaders(body []byte, selector string) []string {
	tableID := strings.TrimPrefix(strings.TrimPrefix(selector, "#"), ".")
	if tableID == "" {
		return nil
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var headers []string
	var found bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && n.Data == "table" {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == tableID {
					headers = extractTHeadHeaders(n)
					found = true
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return headers
}

// extractTHeadHeaders returns the text of all <th> cells in the first
// <thead> of the given table node.
func extractTHeadHeaders(tableNode *html.Node) []string {
	var headers []string
	var inTHead bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "thead":
				inTHead = true
			case "tbody", "tfoot":
				inTHead = false
				return
			case "th":
				if inTHead {
					if t := strings.TrimSpace(nodeText(n)); t != "" {
						headers = append(headers, t)
					}
					return // don't recurse into <th> children
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && n.Data == "thead" {
			inTHead = false
		}
	}
	walk(tableNode)
	return headers
}

// parseDataTablesResponse extracts rows from a DataTables JSON response.
// Supports both array-of-objects and array-of-arrays in the "data" / "aaData"
// keys. Returns the rows and, for object rows, the derived column order.
func parseDataTablesResponse(resp map[string]interface{}) (rows [][]string, colNames []string) {
	var rawData interface{}
	for _, key := range []string{"data", "aaData"} {
		if v, ok := resp[key]; ok {
			rawData = v
			break
		}
	}
	if rawData == nil {
		return
	}
	dataArr, ok := rawData.([]interface{})
	if !ok {
		return
	}

	const maxRows = 200
	for i, item := range dataArr {
		if i >= maxRows {
			break
		}
		switch row := item.(type) {
		case []interface{}:
			cells := make([]string, len(row))
			for j, cell := range row {
				cells[j] = fmt.Sprintf("%v", cell)
			}
			rows = append(rows, cells)
		case map[string]interface{}:
			if i == 0 {
				for k := range row {
					colNames = append(colNames, k)
				}
			}
			cells := make([]string, len(colNames))
			for j, k := range colNames {
				cells[j] = fmt.Sprintf("%v", row[k])
			}
			rows = append(rows, cells)
		}
	}
	return
}

// renderMarkdownTable renders headers and rows as a GitHub-flavoured markdown
// table. Pipe characters inside cells are escaped.
func renderMarkdownTable(headers []string, rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	// Determine column count.
	cols := len(headers)
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}

	escape := func(s string) string {
		s = strings.ReplaceAll(s, "|", "\\|")
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.ReplaceAll(s, "\r", "")
		return strings.TrimSpace(s)
	}

	var sb strings.Builder

	if len(headers) > 0 {
		sb.WriteString("|")
		for i := 0; i < cols; i++ {
			if i < len(headers) {
				sb.WriteString(" " + escape(headers[i]) + " |")
			} else {
				sb.WriteString("  |")
			}
		}
		sb.WriteString("\n|")
		for i := 0; i < cols; i++ {
			sb.WriteString(" --- |")
		}
		sb.WriteString("\n")
	}

	for _, row := range rows {
		sb.WriteString("|")
		for i := 0; i < cols; i++ {
			if i < len(row) {
				sb.WriteString(" " + escape(row[i]) + " |")
			} else {
				sb.WriteString("  |")
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
