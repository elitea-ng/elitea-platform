package mailer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	v2branding "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/branding"
	transport "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/mailer"
)

type brandStub struct{ pack *v2branding.Pack }

func (s brandStub) Current(context.Context) v2branding.Snapshot {
	return v2branding.Snapshot{Pack: s.pack}
}

type recordingTransport struct{ sent []transport.Message }

func (r *recordingTransport) Send(_ context.Context, m transport.Message) error {
	r.sent = append(r.sent, m)
	return nil
}

func ptr(s string) *string { return &s }

func brandedPack() *v2branding.Pack {
	pack := v2branding.DefaultPack()
	pack.Product.Name = "Acme <AI>"
	pack.Product.SenderName = ptr("Acme Platform")
	pack.Product.SupportEmail = ptr("help@acme.example")
	pack.Brand.Hue = "#FF6600"
	pack.Brand.OnBrand = ptr("#101010")
	pack.Assets.LogoEmail = ptr("/api/v2/branding/assets/logo-email/" + strings.Repeat("ab", 32) + ".png")
	pack.Typography.FontFamily = `"Inter", Arial`
	pack.Shape.RadiusMd = 12
	return pack
}

func TestSendInvitation_IsBrandedAndEscaped(t *testing.T) {
	sink := &recordingTransport{}
	composer, err := New(Config{Transport: sink, Brand: brandStub{brandedPack()}, PublicBaseURL: "https://ai.acme.example/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !composer.Configured(context.Background()) {
		t.Fatal("a real transport must report configured")
	}
	err = composer.SendInvitation(context.Background(), Invitation{
		Email: "ada@example.com", Name: "Ada", InvitedBy: "Grace <Ops>", ProjectName: "R&D",
	})
	if err != nil {
		t.Fatalf("SendInvitation: %v", err)
	}
	if len(sink.sent) != 1 {
		t.Fatalf("sent %d messages", len(sink.sent))
	}
	m := sink.sent[0]
	if m.To.Address != "ada@example.com" || m.To.Name != "Ada" || m.FromName != "Acme Platform" {
		t.Fatalf("envelope = %+v", m.To)
	}
	if m.Subject != "You've been invited to Acme <AI>" {
		t.Fatalf("subject = %q", m.Subject)
	}
	for _, want := range []string{
		`<img src="https://ai.acme.example/api/v2/branding/assets/logo-email/`,
		`alt="Acme &lt;AI&gt;"`,
		"Grace &lt;Ops&gt; has invited you",
		"<strong>R&amp;D</strong>",
		"background:#ff6600;color:#101010",
		`href="https://ai.acme.example/"`,
		`mailto:help@acme.example`,
		`font-family:Inter, Arial, Helvetica, Arial, sans-serif`,
		"border-radius:12px",
	} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("html lacks %q:\n%s", want, m.HTML)
		}
	}
	for _, forbidden := range []string{"<AI>", "<Ops>"} {
		if strings.Contains(m.HTML, forbidden) {
			t.Errorf("html carries unescaped %q", forbidden)
		}
	}
	for _, want := range []string{
		"You've been invited to Acme <AI>",
		"Grace <Ops> has invited you to the project \"R&D\".",
		"Sign in with ada@example.com",
		"https://ai.acme.example/",
		"Need help? help@acme.example",
	} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
}

func TestSend_DefaultBrandAndHostileValues(t *testing.T) {
	sink := &recordingTransport{}
	composer, _ := New(Config{Transport: sink, Brand: brandStub{nil}, PublicBaseURL: "https://x.example"})
	if err := composer.SendTest(context.Background(), "ops@example.com"); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if !strings.Contains(sink.sent[0].HTML, ">Elitea AI</p>") || strings.Contains(sink.sent[0].HTML, "<img") {
		t.Fatalf("default brand html:\n%s", sink.sent[0].HTML)
	}

	hostile := v2branding.DefaultPack()
	hostile.Brand.Hue = `#fff" onload="x`
	hostile.Assets.LogoEmail = ptr("https://evil.example/l.png")
	hostile.Typography.FontFamily = "Inter; }"
	hostile.Product.SupportEmail = ptr("Help <help@acme.example>")
	composer, _ = New(Config{Transport: sink, Brand: brandStub{hostile}, PublicBaseURL: "https://x.example"})
	if err := composer.SendModerationDecision(context.Background(), ModerationDecision{Email: "u@example.com", Message: "Your app request has been approved."}); err != nil {
		t.Fatalf("SendModerationDecision: %v", err)
	}
	html := sink.sent[1].HTML
	if strings.Contains(html, "evil.example") || strings.Contains(html, "onload") || strings.Contains(html, "Inter;") || strings.Contains(html, "mailto:") {
		t.Fatalf("hostile values reached the html:\n%s", html)
	}
	if !strings.Contains(html, "background:"+defaultColors.Brand) {
		t.Errorf("brand colour did not fall back to the default")
	}
	if !strings.Contains(html, "Your app request has been approved.") {
		t.Errorf("decision message missing")
	}
}

func TestSend_Refusals(t *testing.T) {
	sink := &recordingTransport{}
	suppressed, _ := New(Config{Transport: sink, Suppressed: true, PublicBaseURL: "https://x.example"})
	if err := suppressed.SendTest(context.Background(), "a@b.example"); !errors.Is(err, ErrSuppressed) || len(sink.sent) != 0 {
		t.Fatalf("shadow mode must refuse without sending: %v, sent %d", err, len(sink.sent))
	}
	if suppressed.Configured(context.Background()) {
		t.Fatal("a suppressed composer must not report configured")
	}

	null, _ := New(Config{})
	if null.Configured(context.Background()) {
		t.Fatal("the null transport must not report configured")
	}
	if err := null.SendTest(context.Background(), "a@b.example"); !errors.Is(err, transport.ErrNotConfigured) {
		t.Fatalf("null transport: %v", err)
	}

	real, _ := New(Config{Transport: sink})
	if err := real.SendTest(context.Background(), "Ada <ada@example.com>"); err == nil {
		t.Fatal("a display-name recipient must be refused")
	}
	if _, err := New(Config{PublicBaseURL: "ftp://x"}); err == nil {
		t.Fatal("a non-http base URL must be refused")
	}
}

// accessNotice is a notice of kind with every field a template can show.
func accessNotice(kind adminaccess.NoticeKind) adminaccess.Notice {
	id := uuid.MustParse("0b5a1c8e-2f41-4c55-9a7e-2d1f3e4a5b6c")
	notice := adminaccess.Notice{
		ID: uuid.New(), Kind: kind, RecipientEmail: "approver@example.com", RecipientName: "Approver",
		ElevationID: id, PersonName: "Grace <Ops>", Level: adminaccess.ElevationLevelAdmin,
		Path: adminaccess.ElevationPathApproved, ReasonCode: adminaccess.ElevationReasonIncident,
		Reason: "the provider is down", ExpiresAt: time.Date(2026, 11, 25, 12, 30, 0, 0, time.UTC),
	}
	switch kind {
	case adminaccess.NoticeRequest:
		notice.ApprovalPath = adminaccess.ElevationApprovalPath(id)
		notice.ApprovalURL = "https://console.acme.example" + notice.ApprovalPath
	case adminaccess.NoticeCriticalChange:
		notice.Action = "identity.sso.trust.manage"
		notice.ActorName = "Ada"
	case adminaccess.NoticeReviewUnauthorized:
		notice.ActorName = "Auditor"
	case adminaccess.NoticeInheritToken:
		notice = adminaccess.Notice{ID: uuid.New(), Kind: kind, RecipientEmail: "owner@example.com",
			Tokens: []adminaccess.NoticeToken{{ID: 1, Name: "ci <deploy>"}, {ID: 2}}, EndsOn: "2027-03-01"}
	}
	return notice
}

// Each access notice renders in HTML and text under the brand view: the
// brand colour and product name are in it, user values are escaped, the
// subject carries no reason text, and the request links to its approval
// page under the public origin the sender built it from (ADR-0059 R2.5a,
// R3.15b).
func TestAccessNoticesRenderWithTheBrandView(t *testing.T) {
	composer, err := New(Config{Transport: &recordingTransport{}, Brand: brandStub{brandedPack()},
		PublicBaseURL: "https://ai.acme.example/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	kinds := AccessNoticeKinds()
	if len(kinds) != 7 {
		t.Fatalf("kinds = %v, want the six elevation kinds and the inherit token notice", kinds)
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			notice := accessNotice(kind)
			subject, html, text, err := composer.RenderAccessNotice(context.Background(), notice)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.HasPrefix(subject, "Acme <AI>: ") || strings.Contains(subject, "provider is down") {
				t.Errorf("subject = %q", subject)
			}
			for _, want := range []string{"background:#ff6600;color:#101010", `alt="Acme &lt;AI&gt;"`} {
				if !strings.Contains(html, want) {
					t.Errorf("html lacks %q:\n%s", want, html)
				}
			}
			if strings.Contains(html, "<Ops>") || strings.Contains(html, "<deploy>") {
				t.Errorf("html carries an unescaped value:\n%s", html)
			}
			if strings.TrimSpace(text) == "" || strings.Contains(text, "&lt;") {
				t.Errorf("text = %q", text)
			}
			if kind == adminaccess.NoticeInheritToken {
				for _, want := range []string{"2027-03-01", "ci <deploy>", "(unnamed)", "https://ai.acme.example/settings/tokens"} {
					if !strings.Contains(text, want) {
						t.Errorf("text lacks %q:\n%s", want, text)
					}
				}
				return
			}
			for _, want := range []string{"Grace <Ops>", "Reason code: incident", "Reason: the provider is down",
				"2026-11-25 12:30 UTC"} {
				if !strings.Contains(text, want) {
					t.Errorf("text lacks %q:\n%s", want, text)
				}
			}
		})
	}

	request := accessNotice(adminaccess.NoticeRequest)
	_, html, text, err := composer.RenderAccessNotice(context.Background(), request)
	if err != nil {
		t.Fatalf("render the request: %v", err)
	}
	link := "https://console.acme.example/admin/app/break-glass/requests/0b5a1c8e-2f41-4c55-9a7e-2d1f3e4a5b6c"
	if !strings.Contains(html, `href="`+link+`"`) || !strings.Contains(text, link) {
		t.Errorf("the request does not link to the approval page under the public origin:\n%s\n%s", html, text)
	}
	// Without a public origin the link falls back to the mailer's resolved
	// origin, never to a relative or request-derived one.
	request.ApprovalURL = ""
	if _, html, _, _ = composer.RenderAccessNotice(context.Background(), request); !strings.Contains(html,
		`href="https://ai.acme.example/admin/app/break-glass/requests/0b5a1c8e-2f41-4c55-9a7e-2d1f3e4a5b6c"`) {
		t.Errorf("fallback link missing:\n%s", html)
	}
	// A recipient who may not read the reason gets the code only.
	request.Reason = ""
	if _, _, text, _ = composer.RenderAccessNotice(context.Background(), request); strings.Contains(text, "Reason: ") ||
		!strings.Contains(text, "Reason code: incident") {
		t.Errorf("reason without permission:\n%s", text)
	}
}

// An activation with approval off, or by the sealed credential, says that no
// approver was involved; an approved one does not.
func TestActivationNoticeSaysWhenNoApproverWasInvolved(t *testing.T) {
	composer, _ := New(Config{Transport: &recordingTransport{}, PublicBaseURL: "https://x.example"})
	for path, want := range map[string]string{
		adminaccess.ElevationPathSelf:     "No approver was involved: approval is switched off.",
		adminaccess.ElevationPathSealed:   "No approver was involved: the sealed credential was used.",
		adminaccess.ElevationPathApproved: "",
	} {
		notice := accessNotice(adminaccess.NoticeActivation)
		notice.Path = path
		_, html, text, err := composer.RenderAccessNotice(context.Background(), notice)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want == "" {
			if strings.Contains(text, "No approver") || strings.Contains(html, "No approver") {
				t.Errorf("%s: says no approver:\n%s", path, text)
			}
			continue
		}
		if !strings.Contains(text, want) || !strings.Contains(html, want) {
			t.Errorf("%s: lacks %q:\n%s\n%s", path, want, text, html)
		}
	}
}

func TestSendAccessNoticeSendsOneMessage(t *testing.T) {
	sink := &recordingTransport{}
	composer, _ := New(Config{Transport: sink, PublicBaseURL: "https://x.example"})
	if err := composer.SendAccessNotice(context.Background(), accessNotice(adminaccess.NoticeActivation)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(sink.sent) != 1 || sink.sent[0].To.Address != "approver@example.com" ||
		sink.sent[0].Subject != "Elitea AI: break-glass access started" {
		t.Fatalf("sent = %+v", sink.sent)
	}
	if err := composer.SendAccessNotice(context.Background(),
		adminaccess.Notice{Kind: "other", RecipientEmail: "a@b.example"}); err == nil {
		t.Fatal("an unknown kind was sent")
	}
}
