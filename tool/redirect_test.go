package tool

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A search backend's key never follows a redirect to another host: a key
// header is dropped, a POST whose body holds the key is not resent. A
// redirect on the same host keeps everything.
func TestSearchClientRedirectKeepsTheKeyHome(t *testing.T) {
	const key = "canary-search-key-0123456789"
	var got []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, fmt.Sprintf("%s %v %s", r.URL, r.Header, b))
		fmt.Fprint(w, "{}")
	}))
	defer other.Close()
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1) // another host name
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, otherURL+"/landed", http.StatusFound)
		case "/away-307":
			http.Redirect(w, r, otherURL+"/landed", http.StatusTemporaryRedirect)
		case "/here":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			if r.Header.Get("X-Subscription-Token") != key {
				t.Errorf("a same-host redirect lost the key header")
			}
			fmt.Fprint(w, "{}")
		}
	}))
	defer home.Close()

	p, err := newSearchProvider(WebSearchConfig{Backend: "brave-free", BraveAPIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	c := p.(*braveProvider).client
	do := func(method, path, body string) error {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, home.URL+path, rd)
		req.Header.Set("X-Subscription-Token", key)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := do("GET", "/away", ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Contains(got[0], key) {
		t.Errorf("the other host got the key: %q", got)
	}
	if err := do("POST", "/away-307", `{"api_key":"`+key+`"}`); err == nil || !strings.Contains(err.Error(), "resends the request body") {
		t.Errorf("307 with a key in the body: %v", err)
	}
	if err := do("GET", "/here", ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("a refused redirect reached the other host: %q", got)
	}
	for _, backend := range []struct{ name, cfgKey string }{{"tavily", "t"}, {"ddgs", ""}} {
		cfg := WebSearchConfig{Backend: backend.name, TavilyAPIKey: "canary-tavily-key"}
		p, err := newSearchProvider(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var cl *http.Client
		switch v := p.(type) {
		case *tavilyProvider:
			cl = v.client
		case *ddgsProvider:
			cl = v.client
		}
		if cl == nil || cl.CheckRedirect == nil {
			t.Errorf("%s: the search client follows redirects with Go's default policy", backend.name)
		}
	}
}
