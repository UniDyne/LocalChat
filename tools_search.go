package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	readability "codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html"
)

// ---------------------------------------------------------------------------
// Shared HTTP helpers
// ---------------------------------------------------------------------------

// webUserAgent is sent with all outbound requests from the search and fetch
// tools. A realistic browser UA avoids bot-detection on most sites.
const webUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

// ---------------------------------------------------------------------------
// Search cache
// ---------------------------------------------------------------------------

type searchCacheEntry struct {
	results []SearchResult
	expiry  time.Time
}

type searchCache struct {
	mu      sync.Mutex
	entries map[string]searchCacheEntry
}

var globalSearchCache = &searchCache{entries: make(map[string]searchCacheEntry)}

const searchCacheTTL = 10 * time.Minute

func (c *searchCache) get(key string) ([]SearchResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expiry) {
		delete(c.entries, key)
		return nil, false
	}
	return e.results, true
}

func (c *searchCache) set(key string, results []SearchResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = searchCacheEntry{results: results, expiry: time.Now().Add(searchCacheTTL)}
}

// ---------------------------------------------------------------------------
// Fetch cache (capacity 50, TTL 30 min, FIFO eviction)
// ---------------------------------------------------------------------------

type fetchCacheEntry struct {
	result FetchResult
	expiry time.Time
}

type fetchCache struct {
	mu      sync.Mutex
	entries map[string]fetchCacheEntry
	order   []string // insertion order for FIFO eviction
}

var globalFetchCache = &fetchCache{entries: make(map[string]fetchCacheEntry)}

const (
	fetchCacheTTL      = 30 * time.Minute
	fetchCacheCapacity = 50
)

func (c *fetchCache) get(key string) (FetchResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expiry) {
		delete(c.entries, key)
		return FetchResult{}, false
	}
	return e.result, true
}

func (c *fetchCache) set(key string, r FetchResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		// Evict oldest entry if at capacity.
		for len(c.entries) >= fetchCacheCapacity && len(c.order) > 0 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = fetchCacheEntry{result: r, expiry: time.Now().Add(fetchCacheTTL)}
}

// ---------------------------------------------------------------------------
// SearchResult / FetchResult types
// ---------------------------------------------------------------------------

// SearchResult is one result returned by the web_search tool.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// FetchResult is what the web_fetch tool returns.
type FetchResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Cached  bool   `json:"cached"`
}

// ---------------------------------------------------------------------------
// DuckDuckGo lite scraper
// ---------------------------------------------------------------------------

// ddgsSearch queries the DuckDuckGo lite endpoint and returns up to n results.
//
// The lite page (https://lite.duckduckgo.com/lite/) is intentionally simple
// HTML — no JavaScript required. Results are in a plain table with predictable
// CSS class names: the anchor is class="result-link" and the snippet cell is
// class="result-snippet". This approach mirrors what the Python duckduckgo-search
// library does, adapted for Go.
func ddgsSearch(query string, n int) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", query)
	form.Set("kl", "us-en")

	req, err := http.NewRequest("POST", "https://lite.duckduckgo.com/lite/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Referer", "https://lite.duckduckgo.com/")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch DDG lite: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DDG lite returned %d", resp.StatusCode)
	}

	results, err := parseDDGLite(resp.Body, n)
	if err != nil {
		return nil, fmt.Errorf("parse DDG lite: %w", err)
	}
	return results, nil
}

// parseDDGLite walks the HTML tree from r and extracts up to n SearchResults.
//
// DDG lite renders results in a table. Each result spans three rows:
//
//	Row 1: number cell + <td class="result-title"><a class="result-link" href="URL">Title</a></td>
//	Row 2: empty cell + <td class="result-snippet">Snippet text</td>
//	Row 3: empty cell + <td class="result-url"><a>display URL</a></td>
//
// We match on the CSS class names, which have been stable for several years.
func parseDDGLite(r io.Reader, n int) ([]SearchResult, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}

	var results []SearchResult
	var cur *SearchResult

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "a":
				if hasClass(node, "result-link") {
					if cur != nil {
						results = append(results, *cur)
					}
					cur = &SearchResult{
						Title: strings.TrimSpace(nodeText(node)),
						URL:   attrVal(node, "href"),
					}
					return // don't descend — title text is already captured
				}
			case "td":
				if hasClass(node, "result-snippet") && cur != nil {
					cur.Snippet = strings.TrimSpace(nodeText(node))
				}
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if cur != nil {
		results = append(results, *cur)
	}

	if n > 0 && len(results) > n {
		results = results[:n]
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Content extraction (web_fetch)
// ---------------------------------------------------------------------------

// fetchPage fetches rawURL, runs it through Readability, converts to Markdown,
// and returns at most maxChars characters of content.
func fetchPage(rawURL string, maxChars int) (FetchResult, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return FetchResult{}, fmt.Errorf("invalid URL: %w", err)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetch URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}

	// Reject non-HTML content types (PDFs, images, etc.) early.
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "xml") {
		return FetchResult{}, fmt.Errorf("unsupported content type %q — only HTML pages are supported", ct)
	}

	// Read body with a 5 MB cap to avoid memory issues on huge pages.
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return FetchResult{}, fmt.Errorf("read response: %w", err)
	}

	// Run Readability on the raw HTML.
	parser := readability.NewParser()
	article, err := parser.Parse(bytes.NewReader(bodyBytes), parsed)
	if err != nil {
		return FetchResult{}, fmt.Errorf("readability parse: %w", err)
	}

	title := article.Title()

	var content string
	if article.Node != nil {
		// Happy path: Readability extracted a clean article node.
		mdBytes, mdErr := htmltomarkdown.ConvertNode(article.Node)
		if mdErr != nil {
			return FetchResult{}, fmt.Errorf("markdown conversion: %w", mdErr)
		}
		content = strings.TrimSpace(string(mdBytes))
	} else {
		// Fallback: Readability found nothing (maybe a single-page app or
		// content-light page). Extract all visible text from the raw HTML.
		content = extractVisibleText(bodyBytes)
	}

	// Tier 2: DataTables AJAX — find any DataTable() calls whose table body
	// is empty in the static HTML and fetch the AJAX endpoint to fill it in.
	if dtMD := dataTablesAjaxMD(bodyBytes, parsed); dtMD != "" {
		if content == "" {
			content = dtMD
		} else {
			content += "\n\n" + dtMD
		}
	}

	// Tier 1: Inline JSON state — when Readability found thin content,
	// scan for __NEXT_DATA__, ld+json, and window state blobs.
	if wordCount(content) < 150 {
		if stateMD := inlineStateMD(bodyBytes); stateMD != "" {
			if content == "" {
				content = stateMD
			} else {
				content += "\n\n" + stateMD
			}
		}
	}

	if maxChars > 0 && len([]rune(content)) > maxChars {
		runes := []rune(content)
		content = string(runes[:maxChars]) + "\n\n[… truncated]"
	}

	return FetchResult{URL: rawURL, Title: title, Content: content}, nil
}

// extractVisibleText is a fallback that strips all HTML tags and returns the
// concatenated visible text nodes, used when Readability yields nothing.
func extractVisibleText(body []byte) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return ""
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "head":
				return // skip these subtrees
			}
		}
		if n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" {
				b.WriteString(t)
				b.WriteByte('\n')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------------------
// HTML helpers (shared by both scrapers)
// ---------------------------------------------------------------------------

// hasClass reports whether an element node carries the given CSS class.
func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}

// attrVal returns the value of the named attribute, or "" if absent.
func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// nodeText concatenates all text nodes under n.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return b.String()
}

// ---------------------------------------------------------------------------
// Tool definitions
// ---------------------------------------------------------------------------

// webSearchTool returns the ToolDef for web_search.
func (a *App) webSearchTool() ToolDef {
	return ToolDef{
		Name:        "web_search",
		Description: "Search the web and return a list of relevant results (title, URL, snippet). Use this when you need current information or facts not in your training data.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":       {"type": "string",  "description": "The search query"},
				"num_results": {"type": "integer", "description": "How many results to return (1–10, default 5)"}
			},
			"required": ["query"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			query, _ := args["query"].(string)
			query = strings.TrimSpace(query)
			if query == "" {
				return "", fmt.Errorf("query must not be empty")
			}
			n := 5
			if v, ok := args["num_results"]; ok {
				switch nv := v.(type) {
				case float64:
					n = int(nv)
				case int:
					n = nv
				}
			}
			if n < 1 {
				n = 1
			}
			if n > 10 {
				n = 10
			}

			engine := a.searchEngine
			if engine == "" {
				engine = "ddgs"
			}
			cacheKey := fmt.Sprintf("%s\x00%s\x00%d", engine, query, n)
			if cached, ok := globalSearchCache.get(cacheKey); ok {
				out, _ := json.Marshal(cached)
				return string(out), nil
			}

			var results []SearchResult
			var err error
			switch engine {
			case "ddgs", "":
				results, err = ddgsSearch(query, n)
			default:
				return "", fmt.Errorf("unsupported search engine %q", engine)
			}
			if err != nil {
				return "", err
			}

			globalSearchCache.set(cacheKey, results)
			out, _ := json.Marshal(results)
			return string(out), nil
		},
	}
}

// webFetchTool returns the ToolDef for web_fetch.
func (a *App) webFetchTool() ToolDef {
	return ToolDef{
		Name:        "web_fetch",
		Description: "Fetch the content of a URL and return it as clean Markdown. Use this after web_search to read the full content of a result, or when given a specific URL to retrieve.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url":       {"type": "string",  "description": "The URL to fetch"},
				"max_chars": {"type": "integer", "description": "Maximum characters to return (default 8000)"}
			},
			"required": ["url"]
		}`),
		Handler: func(a *App, args map[string]any) (string, error) {
			rawURL, _ := args["url"].(string)
			rawURL = strings.TrimSpace(rawURL)
			if rawURL == "" {
				return "", fmt.Errorf("url must not be empty")
			}
			maxChars := 8000
			if v, ok := args["max_chars"]; ok {
				switch mv := v.(type) {
				case float64:
					maxChars = int(mv)
				case int:
					maxChars = mv
				}
			}
			if maxChars < 100 {
				maxChars = 100
			}

			cacheKey := fmt.Sprintf("%s\x00%d", rawURL, maxChars)
			if cached, ok := globalFetchCache.get(cacheKey); ok {
				cached.Cached = true
				out, _ := json.Marshal(cached)
				return string(out), nil
			}

			result, err := fetchPage(rawURL, maxChars)
			if err != nil {
				return "", err
			}

			globalFetchCache.set(cacheKey, result)
			out, _ := json.Marshal(result)
			return string(out), nil
		},
	}
}
