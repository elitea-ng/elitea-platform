package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DecisionPath is the native consent form's action.
const DecisionPath = "/api/v2/auth/native/authorize/decision"

// Step is what a sign-in strategy does with a page it recognises.
type Step struct {
	Form   *Form
	Values url.Values
	Button *Field
	Link   string
}

// Strategy recognises the pages of one sign-in method and answers them. ok is
// false for a page it does not know.
type Strategy func(page *Page) (step Step, ok bool)

// ErrUnrecognisedPage stops a drive at a page no strategy answers.
var ErrUnrecognisedPage = errors.New("no strategy recognises this page")

// Drive walks an authorization from startURL to the app's redirect URI: every
// page is offered to the strategies in order, then to the native consent page
// (answered with decision), then to an auto-submitting form. It returns the
// callback URL and the number of consent pages it answered.
func (b *Browser) Drive(ctx context.Context, startURL, decision string, strategies ...Strategy) (*url.URL, int, error) {
	result, err := b.Navigate(ctx, http.MethodGet, startURL, nil)
	consents := 0
	for steps := 0; ; steps++ {
		if err != nil {
			return nil, consents, err
		}
		if result.Callback != nil {
			return result.Callback, consents, nil
		}
		if steps >= 25 {
			return nil, consents, fmt.Errorf("sign-in did not finish in 25 pages: %s", strings.Join(b.Trail, " → "))
		}
		page := result.Page
		step, ok := Step{}, false
		for _, strategy := range strategies {
			if step, ok = strategy(page); ok {
				break
			}
		}
		if !ok {
			if form, found := ConsentForm(page); found {
				button, has := form.Button("decision", decision)
				if !has {
					return nil, consents, fmt.Errorf("consent page offers no decision=%s button: %s", decision, page.Describe())
				}
				consents++
				step, ok = Step{Form: &form, Button: button}, true
			}
		}
		if !ok && len(page.Forms) == 1 && page.Forms[0].AutoSubmit() {
			step, ok = Step{Form: &page.Forms[0]}, true
		}
		if !ok {
			return nil, consents, fmt.Errorf("%w: %s\ntrail: %s", ErrUnrecognisedPage, page.Describe(),
				strings.Join(b.Trail, " → "))
		}
		if step.Link != "" {
			result, err = b.Navigate(ctx, http.MethodGet, step.Link, nil)
			continue
		}
		result, err = b.Submit(ctx, page, *step.Form, step.Values, step.Button)
	}
}

// ConsentForm finds the native consent form (ADR-0025 decision 3).
func ConsentForm(page *Page) (Form, bool) {
	for _, form := range page.Forms {
		if action, err := url.Parse(form.Action); err == nil && action.Path == DecisionPath {
			return form, true
		}
	}
	return Form{}, false
}

// OIDCMock answers oidc-provider-mock's authorize page: the "Subject" field
// (name `sub`) gets the persona, as apps/elitea-web/e2e/auth.setup.ts does.
func OIDCMock(subject string) Strategy {
	return func(page *Page) (Step, bool) {
		for index := range page.Forms {
			if _, ok := page.Forms[index].Field("sub"); ok {
				return Step{Form: &page.Forms[index], Values: url.Values{"sub": {subject}}}, true
			}
		}
		return Step{}, false
	}
}

// FormLogin answers the Form plane's login page. Its POST must carry exactly
// target, login and password; the page's own `target` is kept.
func FormLogin(login, password string) Strategy {
	return func(page *Page) (Step, bool) {
		for index := range page.Forms {
			form := page.Forms[index]
			_, hasLogin := form.Field("login")
			_, hasPassword := form.Field("password")
			if hasLogin && hasPassword {
				return Step{Form: &page.Forms[index], Values: url.Values{
					"login": {login}, "password": {password},
				}}, true
			}
		}
		return Step{}, false
	}
}

// Chooser answers the single-sign-on chooser by following the provider link
// whose data-provider or href contains want.
func Chooser(want string) Strategy {
	return func(page *Page) (Step, bool) {
		for _, link := range page.Links {
			provider, isProvider := link.Attributes["data-provider"]
			if !isProvider {
				continue
			}
			if strings.Contains(provider, want) || strings.Contains(link.Href, want) {
				return Step{Link: link.Href}, true
			}
		}
		return Step{}, false
	}
}
