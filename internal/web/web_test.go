package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"corpos/internal/mcp"
	"corpos/internal/tool"
)

// fakeFetch returns a Fetcher that serves a canned response, honoring the byte
// limit (so the truncation path is exercised) and short-circuiting to err first.
func fakeFetch(status int, contentType, body string, err error) Fetcher {
	return func(_ context.Context, _, _ string, limit int64) ([]byte, int, string, error) {
		if err != nil {
			return nil, 0, "", err
		}
		b := []byte(body)
		if int64(len(b)) > limit {
			b = b[:limit]
		}
		return b, status, contentType, nil
	}
}

// recordFetch captures what the provider passed to the transport.
type recordFetch struct {
	status            int
	contentType, body string
	gotURL, gotUA     string
	gotLimit          int64
}

func (r *recordFetch) fn() Fetcher {
	return func(_ context.Context, rawURL, ua string, limit int64) ([]byte, int, string, error) {
		r.gotURL, r.gotUA, r.gotLimit = rawURL, ua, limit
		return []byte(r.body), r.status, r.contentType, nil
	}
}

func mustMap(t *testing.T, r tool.Result) map[string]any {
	t.Helper()
	if !r.OK {
		t.Fatalf("result not OK: %+v", r.Value)
	}
	m, ok := r.Value.(map[string]any)
	if !ok {
		t.Fatalf("value not a map: %T", r.Value)
	}
	return m
}

const ddgPage = `
<div class="result">
  <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc&amp;rut=x">The <b>Go</b> docs</a>
  <a class="result__snippet" href="x">Official <b>Go</b> documentation &amp; tutorials.</a>
</div>
<div class="result">
  <a rel="nofollow" class="result__a" href="https://pkg.go.dev/net/http">net/http package</a>
  <a class="result__snippet" href="y">Package http provides HTTP client and server.</a>
</div>`

func TestSearch_ParsesResultsAndUnwrapsRedirect(t *testing.T) {
	t.Parallel()
	rec := &recordFetch{status: 200, contentType: "text/html", body: ddgPage}
	p := New(WithFetcher(rec.fn()), WithSearchURL("https://search.example/html/"))
	res := p.Dispatch(context.Background(), tool.Call{Surface: "web", Action: "search", Params: map[string]any{"query": "go docs"}})
	m := mustMap(t, res)

	if m["query"] != "go docs" || m["count"].(int) != 2 {
		t.Fatalf("query/count = %v / %v", m["query"], m["count"])
	}
	results := m["results"].([]SearchResult)
	if results[0].URL != "https://go.dev/doc" {
		t.Errorf("redirect not unwrapped: %q", results[0].URL)
	}
	if results[0].Title != "The Go docs" {
		t.Errorf("title tags not stripped: %q", results[0].Title)
	}
	if results[0].Snippet != "Official Go documentation & tutorials." {
		t.Errorf("snippet = %q", results[0].Snippet)
	}
	if results[1].URL != "https://pkg.go.dev/net/http" {
		t.Errorf("plain url mangled: %q", results[1].URL)
	}
	// The query and User-Agent must reach the endpoint.
	if !strings.Contains(rec.gotURL, "q=go+docs") {
		t.Errorf("query not in request URL: %q", rec.gotURL)
	}
	if rec.gotUA == "" {
		t.Error("User-Agent not passed to transport")
	}
}

func TestSearch_MaxResultsCaps(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "text/html", ddgPage, nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "search", Params: map[string]any{
		"query": "go", "max_results": float64(1),
	}})
	if n := mustMap(t, res)["count"].(int); n != 1 {
		t.Fatalf("count = %d, want 1 (capped)", n)
	}
}

func TestSearch_IntTypedMaxResults(t *testing.T) {
	t.Parallel()
	// In-process callers (not JSON) may pass an int, not a float64.
	p := New(WithFetcher(fakeFetch(200, "text/html", ddgPage, nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "search", Params: map[string]any{
		"query": "go", "max_results": 1,
	}})
	if n := mustMap(t, res)["count"].(int); n != 1 {
		t.Fatalf("count = %d, want 1 (int max_results honored)", n)
	}
}

func TestSearch_EmptyQueryIsToolError(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "text/html", "", nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "search", Params: map[string]any{"query": "  "}})
	if res.OK || res.ErrorClass != tool.ClassTool {
		t.Fatalf("want tool error, got OK=%v class=%v", res.OK, res.ErrorClass)
	}
}

func TestSearch_TransportErrorIsTransient(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(0, "", "", errors.New("dial fail"))))
	res := p.Dispatch(context.Background(), tool.Call{Action: "search", Params: map[string]any{"query": "x"}})
	if res.OK || res.ErrorClass != tool.ClassTransient {
		t.Fatalf("want transient, got OK=%v class=%v", res.OK, res.ErrorClass)
	}
}

func TestSearch_NoResultsStillSucceeds(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "text/html", "<html>nothing here</html>", nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "search", Params: map[string]any{"query": "x"}})
	if m := mustMap(t, res); m["count"].(int) != 0 {
		t.Fatalf("count = %v, want 0", m["count"])
	}
}

func TestSearch_BadSearchURL(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "", "", nil)), WithSearchURL("://bad"))
	res := p.Dispatch(context.Background(), tool.Call{Action: "search", Params: map[string]any{"query": "x"}})
	if res.OK || res.ErrorClass != tool.ClassFatal {
		t.Fatalf("want fatal on bad search_url, got OK=%v class=%v", res.OK, res.ErrorClass)
	}
}

func TestFetch_HTMLToText(t *testing.T) {
	t.Parallel()
	page := `<html><head><style>.x{color:red}</style><script>doEvil()</script></head>
	<body><h1>Title</h1><p>Hello&nbsp;<b>world</b> &amp; co.</p></body></html>`
	p := New(WithFetcher(fakeFetch(200, "text/html; charset=utf-8", page, nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": "https://example.com"}})
	m := mustMap(t, res)
	text := m["text"].(string)
	if strings.Contains(text, "doEvil") || strings.Contains(text, "color:red") {
		t.Errorf("script/style not stripped: %q", text)
	}
	if !strings.Contains(text, "Title") || !strings.Contains(text, "Hello world & co.") {
		t.Errorf("text = %q", text)
	}
	if m["status"].(int) != 200 || m["truncated"].(bool) {
		t.Errorf("status/truncated = %v / %v", m["status"], m["truncated"])
	}
}

func TestFetch_PlainTextPassThrough(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "text/plain", "raw <not> stripped", nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": "https://e.com/x.txt"}})
	if got := mustMap(t, res)["text"].(string); got != "raw <not> stripped" {
		t.Fatalf("plain text altered: %q", got)
	}
}

func TestFetch_SniffsHTMLWhenContentTypeAbsent(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "", "<!DOCTYPE html><html><body>Hi there</body></html>", nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": "https://e.com"}})
	if got := mustMap(t, res)["text"].(string); got != "Hi there" {
		t.Fatalf("sniffed html not reduced: %q", got)
	}
}

func TestFetch_Truncation(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "text/plain", "abcdefghij", nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{
		"url": "https://e.com", "max_bytes": float64(4),
	}})
	m := mustMap(t, res)
	if m["text"].(string) != "abcd" || !m["truncated"].(bool) || m["bytes"].(int) != 4 {
		t.Fatalf("truncation wrong: text=%q truncated=%v bytes=%v", m["text"], m["truncated"], m["bytes"])
	}
}

func TestFetch_StatusPassThrough(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(404, "text/html", "<html>nope</html>", nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": "https://e.com/missing"}})
	if m := mustMap(t, res); m["status"].(int) != 404 {
		t.Fatalf("status = %v, want 404 (not an error)", m["status"])
	}
}

func TestFetch_RejectsBadURLs(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "", "", nil)))
	for _, raw := range []string{"", "  ", "ftp://x", "file:///etc/passwd", "not a url"} {
		res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": raw}})
		if res.OK || res.ErrorClass != tool.ClassTool {
			t.Errorf("url %q: want tool error, got OK=%v class=%v", raw, res.OK, res.ErrorClass)
		}
	}
}

func TestFetch_TransportErrorIsTransient(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(0, "", "", errors.New("timeout"))))
	res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": "https://e.com"}})
	if res.OK || res.ErrorClass != tool.ClassTransient {
		t.Fatalf("want transient, got OK=%v class=%v", res.OK, res.ErrorClass)
	}
}

func TestDispatch_UnknownActionIsToolError(t *testing.T) {
	t.Parallel()
	p := New(WithFetcher(fakeFetch(200, "", "", nil)))
	res := p.Dispatch(context.Background(), tool.Call{Action: "crawl"})
	if res.OK || res.ErrorClass != tool.ClassTool {
		t.Fatalf("want tool error, got OK=%v class=%v", res.OK, res.ErrorClass)
	}
}

// TestHTTPFetcher_RealTransport exercises the default (non-injected) transport end
// to end against an in-process httptest server: request building, the User-Agent
// header, body read, and metadata extraction — no external network.
func TestHTTPFetcher_RealTransport(t *testing.T) {
	t.Parallel()
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>live page</body></html>")
	}))
	defer srv.Close()

	p := New(WithHTTPClient(srv.Client()), WithUserAgent("corpos-test-ua"))
	res := p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": srv.URL}})
	m := mustMap(t, res)
	if m["status"].(int) != 200 || m["text"].(string) != "live page" {
		t.Fatalf("real fetch wrong: status=%v text=%q", m["status"], m["text"])
	}
	if gotUA != "corpos-test-ua" {
		t.Errorf("server saw UA %q, want corpos-test-ua", gotUA)
	}

	// A closed server makes the real client error → transient.
	srv.Close()
	res = p.Dispatch(context.Background(), tool.Call{Action: "fetch", Params: map[string]any{"url": srv.URL}})
	if res.OK || res.ErrorClass != tool.ClassTransient {
		t.Fatalf("want transient on dead server, got OK=%v class=%v", res.OK, res.ErrorClass)
	}
}

func TestSpecs_ProjectableWebSurface(t *testing.T) {
	t.Parallel()
	specs := New().Specs()
	if len(specs) != 1 || specs[0].Name != Surface {
		t.Fatalf("specs = %+v", specs)
	}
	// The spec must project like an enriched surface: scoping to one action narrows
	// the enum to it (this is the property task 3 relies on for union projection).
	got := mcp.Project(specs, mcp.Scope{Surface: {"search"}})
	if len(got) != 1 {
		t.Fatalf("project len = %d, want 1", len(got))
	}
	props := got[0].InputSchema["properties"].(map[string]any)
	enum := props["action"].(map[string]any)["enum"].([]any)
	if len(enum) != 1 || enum[0] != "search" {
		t.Fatalf("projected enum = %v, want [search]", enum)
	}
}

func TestNew_DefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	d := New()
	if d.searchURL != defaultSearchURL || d.maxResults != defaultMaxResults || d.maxBytes != defaultMaxBytes || d.userAgent != defaultUserAgent {
		t.Fatalf("defaults wrong: %+v", d)
	}
	if d.fetcher == nil {
		t.Fatal("default transport nil")
	}
	// Zero/empty options fall back to defaults.
	z := New(WithSearchURL(""), WithUserAgent(""), WithMaxResults(0), WithMaxBytes(0))
	if z.searchURL != defaultSearchURL || z.userAgent != defaultUserAgent || z.maxResults != defaultMaxResults || z.maxBytes != defaultMaxBytes {
		t.Fatalf("zero options did not fall back: %+v", z)
	}
	o := New(WithSearchURL("u"), WithUserAgent("ua"), WithMaxResults(3), WithMaxBytes(99))
	if o.searchURL != "u" || o.userAgent != "ua" || o.maxResults != 3 || o.maxBytes != 99 {
		t.Fatalf("overrides not applied: %+v", o)
	}
}
