package mailer

import (
	"context"
	"errors"
	"strings"
	"testing"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	transport "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mailer"
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
	if !strings.Contains(sink.sent[0].HTML, ">Elitea</p>") || strings.Contains(sink.sent[0].HTML, "<img") {
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
