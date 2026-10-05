package browser

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A real browser (WebKit, Chromium) sends "Origin: null" on a form POST from a
// page whose referrer policy is no-referrer (Fetch, "serializing a request
// origin"). The stand-in browser must do the same, or the suite passes a
// deployment no real browser can sign in to (Agent Zefir E2E DEF-1: the
// consent page was no-referrer and Safari's decision POST got 403).
func TestFormPostOriginFollowsThePagesReferrerPolicy(t *testing.T) {
	cases := []struct {
		name   string
		header string
		meta   string
		want   string // "self" = the page's own origin
	}{
		{"no policy (strict-origin-when-cross-origin)", "", "", "self"},
		{"header no-referrer", "no-referrer", "", "null"},
		{"meta no-referrer", "", "no-referrer", "null"},
		{"meta overrides header", "no-referrer", "same-origin", "self"},
		{"meta no-referrer overrides header", "same-origin", "no-referrer", "null"},
		{"header list, last valid wins", "no-referrer, bogus, same-origin", "", "self"},
		{"unknown meta ignored", "no-referrer", "bogus", "null"},
		{"legacy never", "", "never", "null"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			mux := http.NewServeMux()
			mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
				if c.header != "" {
					w.Header().Set("Referrer-Policy", c.header)
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				meta := ""
				if c.meta != "" {
					meta = `<meta name="referrer" content="` + c.meta + `">`
				}
				_, _ = io.WriteString(w, `<!doctype html><html><head>`+meta+`</head><body>`+
					`<form action="/post" method="post"><button type="submit" name="d" value="1">Go</button></form></body></html>`)
			})
			mux.HandleFunc("/post", func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Values("Origin")
				w.WriteHeader(http.StatusNoContent)
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			b := New()
			result, err := b.Navigate(context.Background(), http.MethodGet, server.URL+"/page", nil)
			if err != nil || result.Page == nil || len(result.Page.Forms) != 1 {
				t.Fatalf("page: %v %+v", err, result)
			}
			form := result.Page.Forms[0]
			if _, err := b.Submit(context.Background(), result.Page, form, nil, &form.Buttons[0]); err != nil {
				t.Fatal(err)
			}
			want := c.want
			if want == "self" {
				want = server.URL
			}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("Origin = %q, want %q", got, want)
			}
		})
	}
}

func TestSerializedOriginRules(t *testing.T) {
	https := "https://app.example"
	cases := []struct {
		policy, target, want string
	}{
		{"same-origin", "https://app.example/x", https},
		{"same-origin", "https://other.example/x", "null"},
		{"strict-origin", "http://app.example/x", "null"},
		{"strict-origin-when-cross-origin", "https://other.example/x", https},
		{"no-referrer-when-downgrade", "http://other.example/x", "null"},
		{"origin", "http://other.example/x", https},
		{"unsafe-url", "http://other.example/x", https},
		{"no-referrer", "https://app.example/x", "null"},
	}
	for _, c := range cases {
		target, _ := url.Parse(c.target)
		if got := serializeRequestOrigin(c.policy, https, target); got != c.want {
			t.Errorf("%s → %s = %q, want %q", c.policy, c.target, got, c.want)
		}
	}
	if !strings.HasPrefix(defaultReferrerPolicy, "strict-origin-when-cross-origin") {
		t.Fatal(defaultReferrerPolicy)
	}
}
