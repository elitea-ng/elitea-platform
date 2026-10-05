package browser

// Referrer policy, as far as it decides the Origin header of a form POST.
//
// A browser does not always send the submitting document's origin: Fetch
// "serializing a request origin" replaces it with "null" depending on the
// request's referrer policy, which a page sets with a Referrer-Policy header
// and <meta name="referrer"> tags. Under no-referrer EVERY non-GET request
// sends "Origin: null", even to the page's own origin. A stand-in browser that
// always sent the real origin would pass a deployment no real browser can
// sign in to (Agent Zefir E2E DEF-1).

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// defaultReferrerPolicy is the policy when a page sets none.
const defaultReferrerPolicy = "strict-origin-when-cross-origin"

var referrerPolicies = map[string]string{
	"no-referrer":                     "no-referrer",
	"no-referrer-when-downgrade":      "no-referrer-when-downgrade",
	"same-origin":                     "same-origin",
	"origin":                          "origin",
	"strict-origin":                   "strict-origin",
	"origin-when-cross-origin":        "origin-when-cross-origin",
	"strict-origin-when-cross-origin": "strict-origin-when-cross-origin",
	"unsafe-url":                      "unsafe-url",
}

// legacyMetaPolicies are the old keywords a <meta name="referrer"> still
// accepts (HTML, "Standard metadata names").
var legacyMetaPolicies = map[string]string{
	"never":                   "no-referrer",
	"default":                 "strict-origin-when-cross-origin",
	"always":                  "unsafe-url",
	"origin-when-crossorigin": "origin-when-cross-origin",
}

// documentReferrerPolicy is the policy the document applies to its requests:
// the header's last recognised token, then each <meta name="referrer"> in
// document order (the last recognised one wins).
func documentReferrerPolicy(header http.Header, body []byte) string {
	policy := ""
	for _, value := range header.Values("Referrer-Policy") {
		for _, token := range strings.Split(value, ",") {
			if known, ok := referrerPolicies[strings.ToLower(strings.TrimSpace(token))]; ok {
				policy = known
			}
		}
	}
	if strings.Contains(header.Get("Content-Type"), "html") {
		tokenizer := html.NewTokenizer(bytes.NewReader(body))
		for {
			kind := tokenizer.Next()
			if kind == html.ErrorToken {
				break
			}
			if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
				continue
			}
			token := tokenizer.Token()
			if token.Data != "meta" {
				continue
			}
			var name, content string
			for _, attribute := range token.Attr {
				switch strings.ToLower(attribute.Key) {
				case "name":
					name = strings.ToLower(attribute.Val)
				case "content":
					content = strings.ToLower(strings.TrimSpace(attribute.Val))
				}
			}
			if name != "referrer" {
				continue
			}
			if known, ok := referrerPolicies[content]; ok {
				policy = known
			} else if legacy, ok := legacyMetaPolicies[content]; ok {
				policy = legacy
			}
		}
	}
	if policy == "" {
		return defaultReferrerPolicy
	}
	return policy
}

// serializeRequestOrigin is Fetch "serializing a request origin" for a
// navigation (not CORS) request from a document at origin to target.
func serializeRequestOrigin(policy, origin string, target *url.URL) string {
	page, err := url.Parse(origin)
	if err != nil {
		return "null"
	}
	switch policy {
	case "no-referrer":
		return "null"
	case "no-referrer-when-downgrade", "strict-origin", "strict-origin-when-cross-origin":
		if page.Scheme == "https" && target.Scheme != "https" {
			return "null"
		}
	case "same-origin":
		if !strings.EqualFold(page.Scheme, target.Scheme) || !strings.EqualFold(page.Host, target.Host) {
			return "null"
		}
	}
	return origin
}
