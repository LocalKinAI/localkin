package skill

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	searchTimeout    = 10 * time.Second
	searchMaxResults = 10
	// probeTimeout is longer than searchTimeout because a probe is
	// worth waiting for: it deliberately queries engines that may be
	// failing, and SearXNG only reports an engine as down once its own
	// per-engine timeout (up to 60s for a hung upstream) has elapsed.
	// At 10s the whole probe timed out and every engine came back
	// "untested" — the one answer a health check must never give.
	probeTimeout = 75 * time.Second
)

var (
	ddgResultRe  = regexp.MustCompile(`<a rel="nofollow" class="result__a" href="([^"]*)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`<a class="result__snippet"[^>]*>(.*?)</a>`)
	htmlTagRe    = regexp.MustCompile(`<[^>]*>`)
)

type webSearchSkill struct {
	// searxngEndpoint, if non-empty, routes search through a local
	// (or remote) SearXNG meta-search instance. The DDG HTML scrape
	// stays as a fallback for when SearXNG is unreachable.
	// Set via $SEARXNG_ENDPOINT (e.g. http://localhost:8080).
	searxngEndpoint string
}

func NewWebSearchSkill() Skill {
	return &webSearchSkill{
		searxngEndpoint: strings.TrimRight(os.Getenv("SEARXNG_ENDPOINT"), "/"),
	}
}

func (s *webSearchSkill) Name() string { return "web_search" }
func (s *webSearchSkill) Description() string {
	if s.searxngEndpoint != "" {
		return fmt.Sprintf(
			"Search the web via local SearXNG meta-search at %s "+
				"(privacy-respecting, aggregates DuckDuckGo / Google / "+
				"Bing / Yahoo / etc.). Returns titles, URLs, snippets. "+
				"Falls back to DuckDuckGo HTML scrape if SearXNG is "+
				"unreachable.", s.searxngEndpoint)
	}
	return "Search the web using DuckDuckGo and return results with " +
		"titles, URLs, and snippets. No API key needed. Set the " +
		"SEARXNG_ENDPOINT env var (e.g. http://localhost:8080) to " +
		"route through a local SearXNG meta-search for better " +
		"reliability + multi-engine aggregation."
}
func (s *webSearchSkill) ToolDef() json.RawMessage {
	return MakeToolDef("web_search", s.Description(),
		map[string]map[string]string{
			"query": {"type": "string", "description": "The search query"},
		}, []string{"query"})
}

func (s *webSearchSkill) Execute(params map[string]string) (string, error) {
	query := params["query"]
	if query == "" {
		return "", fmt.Errorf("query is required")
	}

	// Backend dispatch: SearXNG first when configured, then DDG as
	// fallback. Both can fail — DDG in particular has been returning
	// HTTP 202 + an empty homepage shell since mid-2026 (anti-bot
	// guard against the html.duckduckgo.com endpoint). When ALL
	// backends fail, the error message tells the model what to do
	// next instead of just dying — the researcher soul's fallback
	// chain points to web_scrape (Scrapling, browser TLS fingerprint,
	// can hit duckduckgo.com directly and parse SERP) as plan B.
	var (
		results      []searchResult
		backend      string
		searxngErr   error
		ddgErr       error
		searxngTried bool
	)
	if s.searxngEndpoint != "" {
		searxngTried = true
		r, err := searchSearXNG(s.searxngEndpoint, query)
		if err == nil {
			results, backend = r, "searxng"
		} else {
			searxngErr = err
			ddg, e := searchDuckDuckGo(query)
			if e == nil {
				results = ddg
				backend = fmt.Sprintf("duckduckgo (searxng unreachable: %v)", err)
			} else {
				ddgErr = e
			}
		}
	} else {
		r, err := searchDuckDuckGo(query)
		if err != nil {
			ddgErr = err
		} else {
			results, backend = r, "duckduckgo"
		}
	}

	// Both backends down — surface an actionable message. The
	// researcher soul's fallback chain reads this and pivots to
	// web_scrape rather than spinning on web_search.
	if results == nil && (searxngErr != nil || ddgErr != nil) {
		var msg strings.Builder
		msg.WriteString("web_search backends all unreachable:\n")
		if searxngTried {
			fmt.Fprintf(&msg, "  - searxng %s: %v\n", s.searxngEndpoint, searxngErr)
			msg.WriteString("    → if you run a local SearXNG, restart it (e.g. `docker restart searxng`).\n")
		}
		if ddgErr != nil {
			fmt.Fprintf(&msg, "  - duckduckgo html scrape: %v\n", ddgErr)
			msg.WriteString("    → DDG's html.duckduckgo.com endpoint blocks non-browser TLS in 2026.\n")
		}
		msg.WriteString("\nFallback: call `web_scrape` with url=\"https://duckduckgo.com/?q=<query>\" — Scrapling has browser TLS fingerprinting and can bypass DDG's bot guard. Or hit a specific source directly via `web_fetch` if you already have a URL in mind.")
		return "", fmt.Errorf("%s", msg.String())
	}

	if len(results) == 0 {
		return "No results found (via " + backend + ").", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for %q (via %s):\n\n", query, backend))
	// Tell the model when the meta-search was running on fumes. A 200
	// with 23 results from wiby and bpb because every major engine is
	// CAPTCHA'd or rate-limited is not a search result, and the model
	// should weigh it (or say so) rather than trust it.
	if st := LastSearchStatus(); st != nil && backend == "searxng" && len(st.Unresponsive) > 0 {
		var down []string
		for _, u := range st.Unresponsive {
			down = append(down, u.Name+" ("+u.Reason+")")
		}
		var used []string
		for e := range st.Engines {
			used = append(used, e)
		}
		sort.Strings(used)
		fmt.Fprintf(&sb, "note: %d engine(s) unavailable — %s. These results come only from: %s. Treat them as weak and prefer a known site via kinbrowser if they look off-topic.\n\n",
			len(down), strings.Join(down, ", "), strings.Join(used, ", "))
	}
	for i, r := range results {
		if i >= searchMaxResults {
			break
		}
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.title, r.url, r.snippet))
	}

	return "---BEGIN UNTRUSTED WEB CONTENT---\n" +
		strings.TrimSpace(sb.String()) +
		"\n---END UNTRUSTED WEB CONTENT---", nil
}

// searchSearXNG queries a local or remote SearXNG meta-search instance
// at /search?q=...&format=json. Response shape:
//
//	{
//	  "query": "...",
//	  "results": [
//	    {"url": "...", "title": "...", "content": "...", ...},
//	    ...
//	  ]
//	}
//
// SearXNG already dedupes across engines and ranks; we just take the
// top N as-is.
func searchSearXNG(endpoint, query string) ([]searchResult, error) {
	raw, err := querySearXNG(endpoint, query, nil)
	st := &SearchStatus{Time: time.Now(), Query: query, Backend: "searxng", Endpoint: endpoint}
	if err != nil {
		st.Error = err.Error()
		recordSearch(st)
		return nil, err
	}
	st.Results = len(raw.Results)
	st.Engines = map[string]int{}
	out := make([]searchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		if r.Engine != "" {
			st.Engines[r.Engine]++
		}
		out = append(out, searchResult{
			title:   strings.TrimSpace(r.Title),
			url:     r.URL,
			snippet: strings.TrimSpace(r.Content),
		})
	}
	st.Unresponsive = raw.unresponsive()
	recordSearch(st)
	return out, nil
}

// searxngResponse is the /search?format=json shape, including the two
// fields that say how healthy the meta-search actually was: which
// engine each result came from, and which engines failed.
type searxngResponse struct {
	Results []struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Content string `json:"content"`
		Engine  string `json:"engine"`
	} `json:"results"`
	// unresponsive_engines is a list of [name, reason] pairs.
	UnresponsiveEngines [][]string `json:"unresponsive_engines"`
}

func (r *searxngResponse) unresponsive() []EngineDown {
	var out []EngineDown
	for _, pair := range r.UnresponsiveEngines {
		if len(pair) == 0 {
			continue
		}
		d := EngineDown{Name: pair[0]}
		if len(pair) > 1 {
			d.Reason = pair[1]
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// querySearXNG runs one /search call. engines, when non-empty, limits
// the fan-out to those engines (the probe uses this so a health check
// doesn't hit twenty upstreams).
func querySearXNG(endpoint, query string, engines []string) (*searxngResponse, error) {
	return querySearXNGTimeout(endpoint, query, engines, searchTimeout)
}

func querySearXNGTimeout(endpoint, query string, engines []string, timeout time.Duration) (*searxngResponse, error) {
	u := endpoint + "/search?format=json&q=" + url.QueryEscape(query)
	if len(engines) > 0 {
		u += "&engines=" + url.QueryEscape(strings.Join(engines, ","))
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng HTTP %d", resp.StatusCode)
	}
	var raw searxngResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("searxng decode: %w", err)
	}
	return &raw, nil
}

// ─── Search health ───────────────────────────────────────────────────

// EngineDown is one engine SearXNG could not use, with its reason
// ("CAPTCHA", "Suspended: too many requests", "access denied"…).
type EngineDown struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// SearchStatus is what the last web_search actually got: which backend
// answered, how many results, from which engines, and which engines
// were down. The UI's search-health indicator reads this; nothing here
// probes anything — it is a record of real traffic.
type SearchStatus struct {
	Time         time.Time      `json:"time"`
	Query        string         `json:"query"`
	Backend      string         `json:"backend"`
	Endpoint     string         `json:"endpoint,omitempty"`
	Results      int            `json:"results"`
	Engines      map[string]int `json:"engines,omitempty"`
	Unresponsive []EngineDown   `json:"unresponsive,omitempty"`
	Error        string         `json:"error,omitempty"`
}

var (
	lastSearchMu sync.Mutex
	lastSearch   *SearchStatus
)

func recordSearch(st *SearchStatus) {
	lastSearchMu.Lock()
	lastSearch = st
	lastSearchMu.Unlock()
}

// LastSearchStatus returns the most recent search outcome, or nil.
func LastSearchStatus() *SearchStatus {
	lastSearchMu.Lock()
	defer lastSearchMu.Unlock()
	if lastSearch == nil {
		return nil
	}
	cp := *lastSearch
	return &cp
}

// SearchEndpoint is the configured SearXNG base URL ("" = none, DDG only).
func SearchEndpoint() string {
	return strings.TrimRight(os.Getenv("SEARXNG_ENDPOINT"), "/")
}

// majorEngines are the ones whose health decides whether search is
// usable. Everything else (wiby, bpb, dictzone…) only fills gaps.
var majorEngines = []string{"google", "bing", "duckduckgo", "brave", "startpage", "qwant", "mojeek", "yahoo", "wikipedia"}

// EngineProbe is one engine's state in a Probe.
type EngineProbe struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Status: ok | down | empty | disabled | untested
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Results int    `json:"results"`
}

// Probe is a live health check of the meta-search.
type Probe struct {
	Endpoint  string        `json:"endpoint"`
	Reachable bool          `json:"reachable"`
	Version   string        `json:"version,omitempty"`
	Error     string        `json:"error,omitempty"`
	Engines   []EngineProbe `json:"engines"`
	// Healthy counts major engines that returned results.
	Healthy int `json:"healthy"`
}

// ProbeSearXNG checks the instance on demand: /config for which major
// engines are enabled, then ONE search restricted to those engines to
// see which answer. Restricted on purpose — a health check that hits
// every enabled engine is how a watchdog becomes the thing that gets
// the IP rate-limited.
func ProbeSearXNG(endpoint string) Probe {
	p := Probe{Endpoint: endpoint}
	if endpoint == "" {
		p.Error = "no SEARXNG_ENDPOINT configured; web_search uses the DuckDuckGo HTML fallback only"
		return p
	}
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get(endpoint + "/config")
	if err != nil {
		p.Error = "unreachable: " + err.Error()
		return p
	}
	var cfg struct {
		Version string `json:"version"`
		Engines []struct {
			Name       string   `json:"name"`
			Enabled    bool     `json:"enabled"`
			Categories []string `json:"categories"`
		} `json:"engines"`
	}
	err = json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if err != nil {
		p.Error = "bad /config: " + err.Error()
		return p
	}
	p.Reachable = true
	p.Version = cfg.Version
	enabled := map[string]bool{}
	for _, e := range cfg.Engines {
		for _, c := range e.Categories {
			if c == "general" {
				enabled[e.Name] = e.Enabled
			}
		}
	}
	var toTest []string
	for _, name := range majorEngines {
		on, known := enabled[name]
		ep := EngineProbe{Name: name, Enabled: on}
		switch {
		case !known:
			ep.Status = "untested"
			ep.Reason = "not installed"
		case !on:
			ep.Status = "disabled"
		default:
			ep.Status = "untested"
			toTest = append(toTest, name)
		}
		p.Engines = append(p.Engines, ep)
	}
	if len(toTest) == 0 {
		return p
	}
	raw, err := querySearXNGTimeout(endpoint, "kinclaw search health", toTest, probeTimeout)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	counts := map[string]int{}
	for _, r := range raw.Results {
		counts[r.Engine]++
	}
	down := map[string]string{}
	for _, d := range raw.unresponsive() {
		down[d.Name] = d.Reason
	}
	for i := range p.Engines {
		e := &p.Engines[i]
		if e.Status != "untested" || !e.Enabled {
			continue
		}
		switch {
		case down[e.Name] != "":
			e.Status, e.Reason = "down", down[e.Name]
		case counts[e.Name] > 0:
			e.Status, e.Results = "ok", counts[e.Name]
			p.Healthy++
		default:
			e.Status = "empty"
		}
	}
	return p
}

type searchResult struct {
	title, url, snippet string
}

func searchDuckDuckGo(query string) ([]searchResult, error) {
	req, err := http.NewRequest("GET", "https://html.duckduckgo.com/html/?q="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Referer", "https://duckduckgo.com/")

	client := &http.Client{Timeout: searchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DuckDuckGo request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DuckDuckGo returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)

	linkMatches := ddgResultRe.FindAllStringSubmatch(html, -1)
	snippetMatches := ddgSnippetRe.FindAllStringSubmatch(html, -1)

	var results []searchResult
	for i, m := range linkMatches {
		if len(m) < 3 {
			continue
		}
		rawURL := m[1]
		title := stripHTML(m[2])

		if parsed, err := url.Parse(rawURL); err == nil {
			if real := parsed.Query().Get("uddg"); real != "" {
				rawURL = real
			}
		}

		snippet := ""
		if i < len(snippetMatches) && len(snippetMatches[i]) >= 2 {
			snippet = stripHTML(snippetMatches[i][1])
		}

		results = append(results, searchResult{title: title, url: rawURL, snippet: snippet})
	}
	return results, nil
}

func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	s = htmlTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}
