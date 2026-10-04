package browser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const consentPage = `<!doctype html><html><body>
<form id="native-consent-form" action="/api/v2/auth/native/authorize/decision" method="post">
  <input type="hidden" name="request" value="h1">
  <input type="hidden" name="uid" value="7">
  <button type="submit" name="decision" value="allow">Continue</button>
  <button type="submit" name="decision" value="deny">Cancel</button>
</form></body></html>`

// A miniature deployment: sign-in page, consent page, then a redirect to a
// private-use scheme the browser must not follow.
func fakeDeployment(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var posted []string
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "binder", Value: "b1", Path: "/"})
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			posted = append(posted, "login:"+r.PostForm.Encode())
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Path: "/"})
			http.Redirect(w, r, "/consent", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<form action="/login" method="post"><label for="l">Login</label>
<input id="l" name="login"><input type="password" name="password"><input type="hidden" name="target" value="t9">
<button class="sign-in-button" type="submit">Sign in</button></form>`)
	})
	mux.HandleFunc("/consent", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "s1" {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, consentPage)
	})
	mux.HandleFunc("/api/v2/auth/native/authorize/decision", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		posted = append(posted, "decision:"+r.PostForm.Encode()+" origin:"+r.Header.Get("Origin"))
		http.Redirect(w, r, "app.example:/cb?code=c1&state=s", http.StatusFound)
	})
	return httptest.NewServer(mux), &posted
}

func TestDriveSignsInConsentsAndStopsAtThePrivateUseRedirect(t *testing.T) {
	server, posted := fakeDeployment(t)
	defer server.Close()
	b := New()
	callback, consents, err := b.Drive(context.Background(), server.URL+"/authorize", "allow", FormLogin("ann", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	if callback.String() != "app.example:/cb?code=c1&state=s" || consents != 1 {
		t.Fatalf("callback %v consents %d", callback, consents)
	}
	want := []string{
		"login:login=ann&password=pw&target=t9",
		"decision:decision=allow&request=h1&uid=7 origin:" + server.URL,
	}
	if strings.Join(*posted, "|") != strings.Join(want, "|") {
		t.Fatalf("posted\n got %q\nwant %q", *posted, want)
	}
}

func TestDriveStopsAtAnUnknownPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<p>nothing to do</p>`)
	}))
	defer server.Close()
	_, _, err := New().Drive(context.Background(), server.URL, "allow")
	if !errors.Is(err, ErrUnrecognisedPage) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseFormsReadsActionsFieldsButtonsAndLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<form method="post"><input id="subject-input" name="sub" aria-label="Subject">
<button type="submit">Authorize</button></form>
<form action="https://idp.example/sso" method="post"><input type="hidden" name="SAMLResponse" value="x"></form>
<a class="provider-button" href="/auth/oidc/login?x=1" data-provider="oidc">OIDC</a>`)
	}))
	defer server.Close()
	result, err := New().Navigate(context.Background(), http.MethodGet, server.URL+"/page?q=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	page := result.Page
	if len(page.Forms) != 2 || len(page.Links) != 1 {
		t.Fatalf("forms %+v links %+v", page.Forms, page.Links)
	}
	first := page.Forms[0]
	if !first.ActionMissing || first.Action != server.URL+"/page?q=1" || first.Fields[0].Label != "Subject" ||
		first.AutoSubmit() || len(first.Buttons) != 1 {
		t.Fatalf("first form %+v", first)
	}
	if !page.Forms[1].AutoSubmit() || page.Forms[1].Action != "https://idp.example/sso" {
		t.Fatalf("second form %+v", page.Forms[1])
	}
	if step, ok := Chooser("oidc")(page); !ok || step.Link != server.URL+"/auth/oidc/login?x=1" {
		t.Fatalf("chooser step %+v %v", step, ok)
	}
	if step, ok := OIDCMock("ann@example.com")(page); !ok || step.Values.Get("sub") != "ann@example.com" {
		t.Fatalf("oidc step %+v %v", step, ok)
	}
}

// A form without an action, on a page reached by POST, re-posts the original
// fields with its own (the oidc-provider-mock shape behind a form_post).
func TestSubmitWithoutActionRepostsThePageFields(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("sub") != "" {
			got = r.PostForm.Encode()
			http.Redirect(w, r, "app:/done", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<form method="post"><input name="sub"></form>`)
	}))
	defer server.Close()
	b := New()
	result, err := b.Navigate(context.Background(), http.MethodPost, server.URL+"/authorize", map[string][]string{"client_id": {"c"}, "state": {"s"}})
	if err != nil {
		t.Fatal(err)
	}
	done, err := b.Submit(context.Background(), result.Page, result.Page.Forms[0], map[string][]string{"sub": {"ann"}}, nil)
	if err != nil || done.Callback == nil {
		t.Fatalf("submit: %+v %v", done, err)
	}
	if got != "client_id=c&state=s&sub=ann" {
		t.Fatalf("re-posted %q", got)
	}
}

func TestDotLocalhostDialsLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Host)
	}))
	defer server.Close()
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	result, err := New().Navigate(context.Background(), http.MethodGet, "http://oidc.localhost:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Page.Body) != "oidc.localhost:"+port {
		t.Fatalf("Host %q", result.Page.Body)
	}
}
