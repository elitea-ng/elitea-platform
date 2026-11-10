// Package svgcheck refuses an SVG document that could run script or fetch a
// resource when a browser renders it in the app's origin. Brand assets
// (ADR-0024 decision 3) and agent avatar stills (ADR-0050 decision 6) share
// this one rule.
//
// It REFUSES, it does not strip. Rewriting XML is where sanitisers go wrong
// (Go's encoder reorders namespaces), and an author whose file fails the
// check can fix it in seconds; an author whose file was silently altered
// cannot tell what changed.
package svgcheck

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Error is a refused document. Reason names the rule; it never quotes more
// of the file than one attribute value.
type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

func refuse(format string, args ...any) error {
	return &Error{Reason: fmt.Sprintf(format, args...)}
}

// Elements an SVG rendered in this origin must not carry. `style` is checked
// by content rather than listed here.
var forbiddenElements = map[string]bool{
	"script": true, "foreignobject": true, "iframe": true, "embed": true,
	"object": true, "link": true, "meta": true, "html": true, "body": true,
	"handler": true, "listener": true,
}

// Check parses the document and returns an *Error for anything that could
// execute or fetch: a script, a foreignObject, an event-handler attribute,
// an external or javascript: reference, a stylesheet with url()/@import, a
// DOCTYPE, or a root element that is not <svg>.
func Check(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	// Entities other than the five predefined ones are a DOCTYPE feature and
	// DOCTYPEs are refused below, so strict mode is right: an undeclared
	// entity is a malformed file, not a file to guess at.
	decoder.Strict = true
	root := ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return refuse("the file is not well-formed SVG: %v", err)
		}
		switch t := token.(type) {
		case xml.Directive:
			return refuse("the SVG carries a DOCTYPE or other directive, which is refused")
		case xml.ProcInst:
			if strings.EqualFold(t.Target, "xml-stylesheet") {
				return refuse("the SVG references an external stylesheet, which is refused")
			}
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if root == "" {
				if local != "svg" {
					return refuse("the file's root element is <%s>, not <svg>", t.Name.Local)
				}
				root = local
			}
			if forbiddenElements[local] {
				return refuse("the SVG contains a <%s> element, which is refused", t.Name.Local)
			}
			for _, attr := range t.Attr {
				name := strings.ToLower(attr.Name.Local)
				if strings.HasPrefix(name, "on") {
					return refuse("the SVG carries an event handler attribute (%s), which is refused", attr.Name.Local)
				}
				if name == "href" || name == "xlink:href" || attr.Name.Space == "http://www.w3.org/1999/xlink" && name == "href" {
					if !safeReference(attr.Value) {
						return refuse("the SVG references %q; only #fragment and data:image references are allowed", attr.Value)
					}
				}
				if name == "style" && unsafeCSS(attr.Value) {
					return refuse("the SVG's inline style loads an external resource, which is refused")
				}
			}
		case xml.CharData:
			// A <style> body: its text arrives as CharData under the element.
			// Cheaper to scan every text run for the dangerous tokens than to
			// track the element stack; text outside <style> that happens to
			// contain "url(" is rare and refusing it is harmless.
			if unsafeCSS(string(t)) {
				return refuse("the SVG's stylesheet loads an external resource (url() or @import), which is refused")
			}
		}
	}
	if root == "" {
		return refuse("the file is not an SVG document")
	}
	return nil
}

func safeReference(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || strings.HasPrefix(v, "#") {
		return true
	}
	lower := strings.ToLower(v)
	return strings.HasPrefix(lower, "data:image/") && !strings.Contains(lower, "svg")
}

func unsafeCSS(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "url(") || strings.Contains(lower, "@import") || strings.Contains(lower, "expression(")
}
