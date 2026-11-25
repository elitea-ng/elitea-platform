package mailer

// The access notices (ADR-0059 decision 5, "Loud"; story R2.5a, issue 2519)
// and the notice to the owners of `inherit` tokens (decision 2; story
// R3.15b, issue 2619). adminaccess.NoticeSender delivers the outbox through
// SendAccessNotice; one template pair per kind, rendered at send time under
// the resolved brand like every other message.
//
// What a message says is bounded by the ADR: it names the person who acted
// and the reason CODE. The reason TEXT is in the body only when the sender
// set it, which it does only for a recipient who may read it in the console
// (an approver or a reviewer), and it is never in a subject.

import (
	"context"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// The console links of the notices that are not a request.
const (
	consoleHomePath    = "/admin/app/"
	tokenSettingsPath  = "/settings/tokens"
	accessTemplateStem = "access_"
)

// accessNoticeCopy is the fixed copy of one kind: the subject after the
// product name, and the label of the action link.
type accessNoticeCopy struct {
	subject string
	action  string
}

var accessNoticeCopies = map[adminaccess.NoticeKind]accessNoticeCopy{
	adminaccess.NoticeRequest:              {"break-glass access requested", "Review the request"},
	adminaccess.NoticeActivation:           {"break-glass access started", "Open the console"},
	adminaccess.NoticeCriticalChange:       {"critical change under break-glass access", "Open the console"},
	adminaccess.NoticeReviewOverdue:        {"break-glass review overdue", "Open the console"},
	adminaccess.NoticeUnreviewedNoReviewer: {"break-glass elevation left without a reviewer", "Open the console"},
	adminaccess.NoticeReviewUnauthorized:   {"break-glass use reviewed as unauthorized", "Open the console"},
	adminaccess.NoticeInheritToken:         {"administration access of your tokens ends", "Manage your tokens"},
}

// AccessNoticeKinds are the notice kinds SendAccessNotice renders.
func AccessNoticeKinds() []adminaccess.NoticeKind {
	return append(append([]adminaccess.NoticeKind{}, adminaccess.ElevationNoticeKinds...), adminaccess.NoticeInheritToken)
}

// SendAccessNotice renders and sends one access notice
// (adminaccess.NoticeMailer).
func (c *Composer) SendAccessNotice(ctx context.Context, notice adminaccess.Notice) error {
	send := c.resolve(ctx)
	view, err := c.accessNoticeView(c.brandView(ctx, send.baseURL), send.baseURL, notice)
	if err != nil {
		return err
	}
	return c.send(ctx, send, accessTemplateStem+string(notice.Kind), notice.RecipientEmail, notice.RecipientName, view)
}

// accessNoticeView fills the brand view with the notice, its subject and its
// link. The request link is the approval page under the public origin the
// sender built it from; without one, under the mailer's resolved origin.
func (c *Composer) accessNoticeView(view View, baseURL string, notice adminaccess.Notice) (View, error) {
	text, ok := accessNoticeCopies[notice.Kind]
	if !ok {
		return View{}, fmt.Errorf("mailer: unknown access notice kind %q", notice.Kind)
	}
	view.Notice = notice
	view.Subject = view.ProductName + ": " + text.subject
	view.ActionLabel = text.action
	switch notice.Kind {
	case adminaccess.NoticeRequest:
		view.ActionURL = notice.ApprovalURL
		if view.ActionURL == "" {
			view.ActionURL = absolute(baseURL, notice.ApprovalPath)
		}
	case adminaccess.NoticeInheritToken:
		view.ActionURL = absolute(baseURL, tokenSettingsPath)
	default:
		view.ActionURL = absolute(baseURL, consoleHomePath)
	}
	return view, nil
}

// RenderAccessNotice renders one notice without sending, for the tests and
// a future preview.
func (c *Composer) RenderAccessNotice(ctx context.Context, notice adminaccess.Notice) (subject, html, text string, err error) {
	send := c.resolve(ctx)
	view, err := c.accessNoticeView(c.brandView(ctx, send.baseURL), send.baseURL, notice)
	if err != nil {
		return "", "", "", err
	}
	html, text, err = c.render(accessTemplateStem+string(notice.Kind), view)
	return view.Subject, html, text, err
}

var _ adminaccess.NoticeMailer = (*Composer)(nil)
