package skill

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestSearchSearXNG hits a tiny in-process server speaking the
// SearXNG /search?format=json contract. Locks in the parse logic
// without depending on a real SearXNG being up on developer machines.
func TestSearchSearXNG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("format"); got != "json" {
			t.Errorf("expected format=json, got %q", got)
		}
		if got := r.URL.Query().Get("q"); got == "" {
			t.Errorf("expected q param, got empty")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"query": "test",
			"results": [
				{"url": "https://a.example/1", "title": "Result A", "content": "  snippet A  "},
				{"url": "https://b.example/2", "title": "Result B", "content": "snippet B"}
			]
		}`))
	}))
	defer srv.Close()

	got, err := searchSearXNG(srv.URL, "test query")
	if err != nil {
		t.Fatalf("searchSearXNG: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].title != "Result A" || got[0].url != "https://a.example/1" {
		t.Errorf("result 0 wrong: %+v", got[0])
	}
	// Whitespace in `content` should be trimmed.
	if got[0].snippet != "snippet A" {
		t.Errorf("result 0 snippet = %q, want trimmed 'snippet A'", got[0].snippet)
	}
}

// TestSearchSearXNG_HTTPError surfaces a non-200 as a clear error.
func TestSearchSearXNG_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := searchSearXNG(srv.URL, "anything")
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention 500, got %v", err)
	}
}

// TestWebSearchSkill_BackendDispatch — when SEARXNG_ENDPOINT is set
// to a working server, the skill uses it; the result string includes
// "(via searxng)". With no env var set, the description falls back
// to mentioning DuckDuckGo.
func TestWebSearchSkill_BackendDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"url":"https://x.example","title":"X","content":"X content"}]}`))
	}))
	defer srv.Close()

	old := os.Getenv("SEARXNG_ENDPOINT")
	t.Cleanup(func() { os.Setenv("SEARXNG_ENDPOINT", old) })
	os.Setenv("SEARXNG_ENDPOINT", srv.URL)

	s := NewWebSearchSkill()
	if !strings.Contains(s.Description(), "SearXNG") {
		t.Errorf("Description should mention SearXNG when env var set, got: %s", s.Description())
	}
	out, err := s.Execute(map[string]string{"query": "anything"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "(via searxng)") {
		t.Errorf("expected 'via searxng' tag in result, got %s", out)
	}
	if !strings.Contains(out, "https://x.example") {
		t.Errorf("expected mocked URL in result")
	}
}

func TestWebSearchSkill_Name(t *testing.T) {
	s := NewWebSearchSkill()
	if s.Name() != "web_search" {
		t.Errorf("Name() = %q, want web_search", s.Name())
	}
}

func TestWebSearchSkill_ToolDef(t *testing.T) {
	s := NewWebSearchSkill()
	def := s.ToolDef()
	if def == nil {
		t.Fatal("ToolDef() returned nil")
	}
	raw := string(def)
	if !strings.Contains(raw, "web_search") {
		t.Errorf("ToolDef missing web_search: %s", raw)
	}
	if !strings.Contains(raw, "query") {
		t.Errorf("ToolDef missing query parameter: %s", raw)
	}
}

func TestWebSearchSkill_EmptyQuery(t *testing.T) {
	s := NewWebSearchSkill()
	_, err := s.Execute(map[string]string{"query": ""})
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"<b>bold</b>", "bold"},
		{"plain text", "plain text"},
		{"<a href='#'>link</a>", "link"},
		{"<b>Go</b> is <i>fast</i>", "Go is fast"},
		{"  spaces  ", "spaces"},
	}
	for _, tt := range tests {
		got := stripHTML(tt.input)
		if got != tt.want {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSearXNG_RecordsStatusAndNotesDownEngines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			fmt.Fprint(w, `{"results":[{"url":"https://x","title":"Mountain","content":"c","engine":"wiby"},{"url":"https://y","title":"B","content":"d","engine":"bing"}],"unresponsive_engines":[["duckduckgo","CAPTCHA"],["brave","Suspended: too many requests"]]}`)
		case "/config":
			fmt.Fprint(w, `{"version":"2026.2.16","engines":[{"name":"google","enabled":false,"categories":["general"]},{"name":"bing","enabled":true,"categories":["general"]},{"name":"duckduckgo","enabled":true,"categories":["general"]},{"name":"wiby","enabled":true,"categories":["general"]}]}`)
		}
	}))
	defer srv.Close()

	s := &webSearchSkill{searxngEndpoint: srv.URL}
	out, err := s.Execute(map[string]string{"query": "mountain view farmers market"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "note: 2 engine(s) unavailable") || !strings.Contains(out, "duckduckgo (CAPTCHA)") || !strings.Contains(out, "come only from: bing, wiby") {
		t.Fatalf("missing health note:\n%s", out)
	}
	st := LastSearchStatus()
	if st == nil || st.Results != 2 || st.Engines["wiby"] != 1 || len(st.Unresponsive) != 2 || st.Backend != "searxng" {
		t.Fatalf("status: %+v", st)
	}

	p := ProbeSearXNG(srv.URL)
	if !p.Reachable || p.Version != "2026.2.16" {
		t.Fatalf("probe: %+v", p)
	}
	byName := map[string]EngineProbe{}
	for _, e := range p.Engines {
		byName[e.Name] = e
	}
	if byName["google"].Status != "disabled" || byName["duckduckgo"].Status != "down" || byName["bing"].Status != "ok" || byName["startpage"].Status != "untested" {
		t.Fatalf("engine states: %+v", byName)
	}
	if p.Healthy != 1 {
		t.Fatalf("healthy=%d", p.Healthy)
	}
}
