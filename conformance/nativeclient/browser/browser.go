// Package browser is a headless stand-in for the system browser an RFC 8252
// native app opens: a cookie jar, redirect following that stops at the app's
// private-use redirect URI, and HTML form submission. It runs no JavaScript,
// so it drives exactly the server-rendered pages a real browser would see,
// and it has no bypass of sign-in or consent (the ADR-0017 lesson).
package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// maxHops bounds one navigation's redirect chain.
const maxHops = 20

// Browser is one browser profile: its cookies persist across navigations.
type Browser struct {
	http *http.Client
	// Trail records every request URL, for failure messages.
	Trail []string
	// origin and referrerPolicy describe the document whose form is being
	// submitted: the next POST's Origin is that origin, serialized under that
	// policy as a browser does it (referrer.go).
	origin         string
	referrerPolicy string
}

// New returns a browser with an empty cookie jar. Every `*.localhost` host
// dials loopback, as Chromium and WebKit do; the E2E stack's OIDC issuer is
// `oidc.localhost:<port>` (memory e2e-oidc-host-resolution).
func New() *Browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err == nil && strings.HasSuffix(strings.ToLower(host), ".localhost") {
			address = net.JoinHostPort("127.0.0.1", port)
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &Browser{http: &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Cookies returns the cookies the jar would send to rawURL.
func (b *Browser) Cookies(rawURL string) []*http.Cookie {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	return b.http.Jar.Cookies(parsed)
}

// HTTPClient exposes the browser's client (and jar) for cookie-authenticated
// API calls, e.g. an administrator's console session.
func (b *Browser) HTTPClient() *http.Client { return b.http }

// Page is where a navigation came to rest.
type Page struct {
	URL    *url.URL
	Status int
	Header http.Header
	Body   []byte
	// Method and Posted describe the request that produced the page: a form
	// with no action posts back to the page's own URL, and a page reached by
	// POST needs that POST's fields again.
	Method string
	Posted url.Values
	Forms  []Form
	Links  []Link
}

// Callback is set when the navigation ended at a non-http(s) redirect: the
// native app's redirect URI.
type Result struct {
	Page     *Page
	Callback *url.URL
}

// ErrTooManyHops is a redirect loop.
var ErrTooManyHops = errors.New("too many redirects")

// Navigate requests rawURL and follows redirects until a page or a callback.
func (b *Browser) Navigate(ctx context.Context, method, rawURL string, form url.Values) (Result, error) {
	for hop := 0; hop < maxHops; hop++ {
		target, err := url.Parse(rawURL)
		if err != nil {
			return Result{}, err
		}
		if target.Scheme != "http" && target.Scheme != "https" {
			return Result{Callback: target}, nil
		}
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader(form.Encode())
		}
		request, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		if err != nil {
			return Result{}, err
		}
		request.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
		request.Header.Set("User-Agent", "elitea-native-conformance/1 (headless system browser)")
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			// A browser sends the submitting document's origin with a form
			// POST; the native decision route compares it with the public
			// origin.
			if b.origin != "" {
				request.Header.Set("Origin", serializeRequestOrigin(b.referrerPolicy, b.origin, target))
			}
		}
		b.Trail = append(b.Trail, method+" "+rawURL)
		response, err := b.http.Do(request)
		if err != nil {
			return Result{}, fmt.Errorf("%s %s: %w", method, rawURL, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		_ = response.Body.Close()
		if readErr != nil {
			return Result{}, readErr
		}
		switch response.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location := response.Header.Get("Location")
			if location == "" {
				return Result{}, fmt.Errorf("%s %s: %d without Location", method, rawURL, response.StatusCode)
			}
			next, err := target.Parse(location)
			if err != nil {
				return Result{}, fmt.Errorf("%s %s: bad Location %q: %w", method, rawURL, location, err)
			}
			if response.StatusCode != http.StatusTemporaryRedirect &&
				response.StatusCode != http.StatusPermanentRedirect {
				method, form = http.MethodGet, nil
			}
			rawURL = next.String()
			continue
		}
		page := &Page{
			URL: target, Status: response.StatusCode, Header: response.Header, Body: raw,
			Method: method, Posted: form,
		}
		if strings.Contains(response.Header.Get("Content-Type"), "html") {
			page.Forms, page.Links = parseDocument(target, raw)
		}
		return Result{Page: page}, nil
	}
	return Result{}, fmt.Errorf("%w: %s", ErrTooManyHops, strings.Join(b.Trail, " → "))
}

// Submit posts (or gets) a form. values override the form's own fields; button
// is the submit button that was "clicked" (nil for none).
func (b *Browser) Submit(ctx context.Context, page *Page, form Form, values url.Values, button *Field) (Result, error) {
	fields := url.Values{}
	action := form.Action
	if form.ActionMissing && page.Method == http.MethodPost {
		// No action: the browser posts back to the document URL. A document
		// that was itself the answer to a POST lost that POST's fields from
		// its URL, so a faithful re-post carries them again.
		for name, list := range page.Posted {
			fields[name] = append([]string(nil), list...)
		}
	}
	for _, field := range form.Fields {
		if field.Name != "" {
			fields.Set(field.Name, field.Value)
		}
	}
	for name, list := range values {
		fields[name] = list
	}
	if button != nil && button.Name != "" {
		fields.Set(button.Name, button.Value)
	}
	method := strings.ToUpper(form.Method)
	b.origin = page.URL.Scheme + "://" + page.URL.Host
	b.referrerPolicy = documentReferrerPolicy(page.Header, page.Body)
	defer func() { b.origin, b.referrerPolicy = "", "" }()
	if method != http.MethodPost {
		target, err := url.Parse(action)
		if err != nil {
			return Result{}, err
		}
		target.RawQuery = fields.Encode()
		return b.Navigate(ctx, http.MethodGet, target.String(), nil)
	}
	return b.Navigate(ctx, http.MethodPost, action, fields)
}

// Describe summarises a page for a failure message.
func (p *Page) Describe() string {
	text := bytes.TrimSpace(p.Body)
	if len(text) > 800 {
		text = append(text[:800:800], []byte("…")...)
	}
	return fmt.Sprintf("%s %s → HTTP %d, %d form(s), %d link(s): %s",
		p.Method, p.URL, p.Status, len(p.Forms), len(p.Links), text)
}
