// Package web is the owned web MCP surface for Corp-OS: the web.search and
// web.fetch actions that cannibalize Claude Code's WebSearch / WebFetch tools
// (cannibalization map §3.1). It implements tool.Provider, so the aggregator
// mounts it as a second server beside the toolkit-server — the first genuinely
// new tool-surface and the first multi-server consumer (§5#3).
//
// It is sans-IO testable: the HTTP transport is an injectable Fetcher, so the whole
// search-parse and fetch-to-text path runs against canned responses with no
// network. The default transport hits a public search endpoint (DuckDuckGo's HTML
// results page by default; point search_url at a SearXNG instance to swap it).
//
// No third-party dependency: results are extracted and HTML is reduced to text
// with the standard library alone (net/http, net/url, html, regexp), keeping the
// go.mod minimal and the build CGo-free.
package web

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"corpos/internal/mcp"
	"corpos/internal/tool"
)

// Surface is the surface name the model addresses (web.search / web.fetch).
const Surface = "web"

// Defaults for the web surface.
const (
	defaultSearchURL  = "https://html.duckduckgo.com/html/"
	defaultUserAgent  = "corpos-web/1.0"
	defaultMaxResults = 10
	defaultMaxBytes   = 512 * 1024 // 512 KiB fetch-body cap
	searchPageCap     = 2 << 20    // 2 MiB cap on a search results page
)

// Fetcher is the injectable HTTP seam: GET rawURL with the User-Agent header and
// return up to limit bytes of the body plus its status and content-type. Returning
// already-read bytes (not an *http.Response) keeps tests from juggling response
// bodies — the repo's sans-IO transport shape (cf. mcp.Transport) — and confines
// body-closing to the one real implementation. Tests inject a fake.
type Fetcher func(ctx context.Context, rawURL, userAgent string, limit int64) (body []byte, status int, contentType string, err error)

// Provider is the web MCP surface. It dispatches web.search and web.fetch.
type Provider struct {
	searchURL  string
	userAgent  string
	fetcher    Fetcher
	maxResults int
	maxBytes   int64
}

// Option configures a Provider.
type Option func(*Provider)

// WithSearchURL overrides the search endpoint (e.g. a SearXNG instance).
func WithSearchURL(u string) Option { return func(p *Provider) { p.searchURL = u } }

// WithUserAgent overrides the outbound User-Agent header.
func WithUserAgent(ua string) Option { return func(p *Provider) { p.userAgent = ua } }

// WithHTTPClient drives the default transport with a specific *http.Client (e.g.
// a custom timeout/proxy).
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.fetcher = newHTTPFetcher(c) }
}

// WithFetcher injects the HTTP transport directly (tests pass a fake).
func WithFetcher(f Fetcher) Option { return func(p *Provider) { p.fetcher = f } }

// WithMaxResults caps the number of search results returned.
func WithMaxResults(n int) Option { return func(p *Provider) { p.maxResults = n } }

// WithMaxBytes caps the fetch response body read (bytes).
func WithMaxBytes(n int64) Option { return func(p *Provider) { p.maxBytes = n } }

// New builds a web Provider. Empty/zero options fall back to the defaults; an
// uninjected transport uses a plain *http.Client with a bounded timeout.
func New(opts ...Option) *Provider {
	p := &Provider{
		searchURL:  defaultSearchURL,
		userAgent:  defaultUserAgent,
		maxResults: defaultMaxResults,
		maxBytes:   defaultMaxBytes,
	}
	for _, o := range opts {
		o(p)
	}
	if p.searchURL == "" {
		p.searchURL = defaultSearchURL
	}
	if p.userAgent == "" {
		p.userAgent = defaultUserAgent
	}
	if p.maxResults <= 0 {
		p.maxResults = defaultMaxResults
	}
	if p.maxBytes <= 0 {
		p.maxBytes = defaultMaxBytes
	}
	if p.fetcher == nil {
		p.fetcher = newHTTPFetcher(&http.Client{Timeout: 30 * time.Second})
	}
	return p
}

// newHTTPFetcher is the default Fetcher: a real GET against c, reading at most
// limit bytes. It owns the response body's lifecycle (closed before return), so no
// *http.Response escapes — the whole point of the Fetcher seam.
func newHTTPFetcher(c *http.Client) Fetcher {
	return func(ctx context.Context, rawURL, userAgent string, limit int64) ([]byte, int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, 0, "", fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.Do(req)
		if err != nil {
			return nil, 0, "", fmt.Errorf("GET %s: %w", rawURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
		if err != nil {
			return nil, 0, "", fmt.Errorf("read body: %w", err)
		}
		return data, resp.StatusCode, resp.Header.Get("Content-Type"), nil
	}
}

// ensure *Provider satisfies the provider seam at compile time.
var _ tool.Provider = (*Provider)(nil)

// Specs returns the single web surface spec, built through mcp.EnvelopeSpec so its
// action enum has the same shape an enriched toolkit surface does — which is what
// lets mcp.Project narrow web exactly like any other surface (task 3, the union
// projection). Actions are listed sorted to match the enum order.
func (p *Provider) Specs() []tool.Spec {
	entries := []string{
		"fetch(url, max_bytes?) — fetch a URL and return its readable text (HTML stripped to plain text)",
		"search(query, max_results?) — web search; returns ranked results as {title, url, snippet}",
	}
	return []tool.Spec{
		mcp.EnvelopeSpec(Surface,
			"Owned web surface: search the web and fetch page text. Use for current/external information the substrate does not hold.",
			[]string{"fetch", "search"}, entries),
	}
}

// Dispatch routes a web call to its action handler. Like every provider it never
// returns a Go error out of band — failures come back as a tool.Result with a
// non-empty ErrorClass so the loop folds them into the transcript.
func (p *Provider) Dispatch(ctx context.Context, call tool.Call) tool.Result {
	switch call.Action {
	case "search":
		return p.search(ctx, call)
	case "fetch":
		return p.fetch(ctx, call)
	default:
		return tool.Fail(call, tool.ClassTool, fmt.Sprintf("unknown web action %q (want search|fetch)", call.Action), 0)
	}
}

// SearchResult is one ranked web result.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// search runs a web search and returns ranked results. A missing/blank query is a
// tool error; a transport failure is transient; a results page that parses to zero
// results still succeeds (empty result set is a valid answer, not an error).
func (p *Provider) search(ctx context.Context, call tool.Call) tool.Result {
	start := time.Now()
	query := strings.TrimSpace(stringParam(call.Params, "query"))
	if query == "" {
		return tool.Fail(call, tool.ClassTool, "search requires a non-empty 'query'", since(start))
	}
	max := p.maxResults
	if n, ok := intParam(call.Params, "max_results"); ok && n > 0 {
		max = n
	}

	u, err := url.Parse(p.searchURL)
	if err != nil {
		return tool.Fail(call, tool.ClassFatal, fmt.Sprintf("bad search_url: %v", err), since(start))
	}
	q := u.Query()
	q.Set("q", query)
	u.RawQuery = q.Encode()

	body, _, _, err := p.fetcher(ctx, u.String(), p.userAgent, searchPageCap)
	if err != nil {
		return tool.Fail(call, tool.ClassTransient, err.Error(), since(start))
	}
	results := parseResults(string(body), max)
	out := map[string]any{
		"query":   query,
		"count":   len(results),
		"results": results,
	}
	return tool.Result{Call: call, OK: true, Value: out, ErrorClass: tool.ClassNone, LatencyMS: since(start)}
}

// fetch GETs a URL and returns its readable text. Non-HTTP(S) schemes and blank
// URLs are tool errors; a transport failure is transient. A non-2xx status is NOT
// an error — the status and whatever body came back are returned so the caller can
// reason about it (matching how a fetch tool surfaces a 404 page).
func (p *Provider) fetch(ctx context.Context, call tool.Call) tool.Result {
	start := time.Now()
	raw := strings.TrimSpace(stringParam(call.Params, "url"))
	if raw == "" {
		return tool.Fail(call, tool.ClassTool, "fetch requires a 'url'", since(start))
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return tool.Fail(call, tool.ClassTool, fmt.Sprintf("fetch needs an http(s) url, got %q", raw), since(start))
	}
	limit := p.maxBytes
	if n, ok := intParam(call.Params, "max_bytes"); ok && n > 0 {
		limit = int64(n)
	}

	body, status, contentType, err := p.fetcher(ctx, u.String(), p.userAgent, limit+1) // +1 byte to detect truncation
	if err != nil {
		return tool.Fail(call, tool.ClassTransient, err.Error(), since(start))
	}
	truncated := int64(len(body)) > limit
	if truncated {
		body = body[:limit]
	}
	text := string(body)
	if isHTML(contentType, body) {
		text = htmlToText(text)
	}
	out := map[string]any{
		"url":          u.String(),
		"status":       status,
		"content_type": contentType,
		"text":         text,
		"bytes":        len(body),
		"truncated":    truncated,
	}
	return tool.Result{Call: call, OK: true, Value: out, ErrorClass: tool.ClassNone, LatencyMS: since(start)}
}

// since returns elapsed milliseconds from start.
func since(start time.Time) int64 { return time.Since(start).Milliseconds() }

// stringParam reads a string param, tolerating absence/wrong-type as "".
func stringParam(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}

// intParam reads an integer-valued param. JSON numbers decode to float64, so it
// accepts float64 (and int for in-process callers); the bool reports presence.
func intParam(params map[string]any, key string) (int, bool) {
	switch v := params[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

// --- HTML/result extraction (stdlib only) ---

var (
	// DuckDuckGo HTML results: each result title is an <a class="result__a"> whose
	// href is the (often redirect-wrapped) target; the snippet is a sibling
	// <a class="result__snippet">. Class precedes href in DDG's markup.
	resultLinkRe = regexp.MustCompile(`(?is)<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	resultSnipRe = regexp.MustCompile(`(?is)<a[^>]*class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
	scriptStyle  = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]+>`)
	// \p{Z} also folds Unicode spaces (e.g. &nbsp; → U+00A0) that \s misses.
	wsRe = regexp.MustCompile(`[\s\p{Z}]+`)
)

// parseResults extracts up to max results from a search results page, zipping
// title links with snippet blocks by position (a missing snippet yields "").
func parseResults(page string, max int) []SearchResult {
	links := resultLinkRe.FindAllStringSubmatch(page, -1)
	snips := resultSnipRe.FindAllStringSubmatch(page, -1)
	out := make([]SearchResult, 0, len(links))
	for i, m := range links {
		if max > 0 && len(out) >= max {
			break
		}
		snippet := ""
		if i < len(snips) {
			snippet = cleanText(snips[i][1])
		}
		out = append(out, SearchResult{
			Title:   cleanText(m[2]),
			URL:     resolveURL(m[1]),
			Snippet: snippet,
		})
	}
	return out
}

// resolveURL unwraps a DuckDuckGo redirect (//duckduckgo.com/l/?uddg=<encoded>) to
// the real target; a plain URL is returned unchanged (with entities unescaped).
func resolveURL(href string) string {
	href = html.UnescapeString(strings.TrimSpace(href))
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return href
}

// isHTML decides whether a body should be reduced to text: an explicit HTML
// content-type, or (when the type is absent/ambiguous) a body that opens like
// markup.
func isHTML(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	if contentType == "" || strings.Contains(strings.ToLower(contentType), "octet-stream") {
		head := strings.ToLower(strings.TrimSpace(string(body)))
		return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
	}
	return false
}

// htmlToText reduces an HTML document to readable text: drop script/style blocks,
// strip remaining tags, unescape entities, and collapse whitespace.
func htmlToText(s string) string {
	s = scriptStyle.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// cleanText strips tags from an inline fragment, unescapes entities, and collapses
// whitespace — for titles/snippets that may contain <b> highlight tags.
func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}
