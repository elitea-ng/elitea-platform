package browser

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Field is one form control that submits a value.
type Field struct {
	Name  string
	Value string
	Type  string
	ID    string
	Label string
}

// Form is one <form>.
type Form struct {
	ID            string
	Action        string
	ActionMissing bool
	Method        string
	Fields        []Field
	Buttons       []Field
}

// Link is one <a href>.
type Link struct {
	Href       string
	Text       string
	Attributes map[string]string
}

// Field returns the named field.
func (f Form) Field(name string) (Field, bool) {
	for _, field := range f.Fields {
		if field.Name == name {
			return field, true
		}
	}
	return Field{}, false
}

// Button returns the submit button with this name and value.
func (f Form) Button(name, value string) (*Field, bool) {
	for index := range f.Buttons {
		if f.Buttons[index].Name == name && f.Buttons[index].Value == value {
			return &f.Buttons[index], true
		}
	}
	return nil, false
}

// AutoSubmit is a form a page posts by script on load: only hidden inputs.
// Without JavaScript this headless browser submits it itself, as the page's
// <noscript> fallback button would.
func (f Form) AutoSubmit() bool {
	if len(f.Fields) == 0 {
		return false
	}
	for _, field := range f.Fields {
		if field.Type != "hidden" {
			return false
		}
	}
	return true
}

func attr(node *html.Node, name string) (string, bool) {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return attribute.Val, true
		}
	}
	return "", false
}

func textOf(node *html.Node) string {
	var buffer bytes.Buffer
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			buffer.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(buffer.String()), " ")
}

// parseDocument extracts the forms and links of an HTML document, resolving
// every URL against base.
func parseDocument(base *url.URL, body []byte) ([]Form, []Link) {
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, nil
	}
	labels := map[string]string{}
	var forms []Form
	var links []Link
	var current *Form
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "label":
				if target, ok := attr(node, "for"); ok {
					labels[target] = textOf(node)
				}
			case "form":
				action, present := attr(node, "action")
				method, _ := attr(node, "method")
				if method == "" {
					method = "get"
				}
				id, _ := attr(node, "id")
				resolved := base.String()
				if present && action != "" {
					if parsed, err := base.Parse(action); err == nil {
						resolved = parsed.String()
					}
				}
				forms = append(forms, Form{
					ID: id, Action: resolved, ActionMissing: !present || action == "", Method: method,
				})
				previous := current
				current = &forms[len(forms)-1]
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					walk(child)
				}
				current = previous
				return
			case "input":
				if current != nil {
					field := fieldOf(node)
					field.Type = strings.ToLower(field.Type)
					if field.Type == "" {
						field.Type = "text"
					}
					switch field.Type {
					case "submit", "image":
						current.Buttons = append(current.Buttons, field)
					case "button", "reset", "file":
					default:
						current.Fields = append(current.Fields, field)
					}
				}
			case "button":
				if current != nil {
					field := fieldOf(node)
					if field.Type == "" || strings.EqualFold(field.Type, "submit") {
						field.Type = "submit"
						field.Label = textOf(node)
						current.Buttons = append(current.Buttons, field)
					}
				}
			case "a":
				if href, ok := attr(node, "href"); ok {
					attributes := map[string]string{}
					for _, attribute := range node.Attr {
						attributes[attribute.Key] = attribute.Val
					}
					if parsed, err := base.Parse(href); err == nil {
						links = append(links, Link{Href: parsed.String(), Text: textOf(node), Attributes: attributes})
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	for formIndex := range forms {
		for fieldIndex := range forms[formIndex].Fields {
			field := &forms[formIndex].Fields[fieldIndex]
			if field.Label == "" && field.ID != "" {
				field.Label = labels[field.ID]
			}
		}
	}
	return forms, links
}

func fieldOf(node *html.Node) Field {
	var field Field
	field.Name, _ = attr(node, "name")
	field.Value, _ = attr(node, "value")
	field.Type, _ = attr(node, "type")
	field.ID, _ = attr(node, "id")
	if label, ok := attr(node, "aria-label"); ok {
		field.Label = label
	} else if placeholder, ok := attr(node, "placeholder"); ok {
		field.Label = placeholder
	}
	return field
}
