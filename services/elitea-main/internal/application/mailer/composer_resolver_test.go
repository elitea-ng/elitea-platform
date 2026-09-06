package mailer

// The RESOLVER seam (gap G7): the composer asks what to send through on every
// send, so a save on the admin E-mail page reaches the next message with no
// restart. That property is the whole point of the change, and it is invisible
// to any test that constructs a composer with a fixed transport.

import (
	"context"
	"errors"
	"strings"
	"testing"

	transport "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mailer"
)

// stubResolver stands in for `internal/emailsettings`'s resolver: it decides
// the transport and the origin for ONE send, and it can change its mind
// between sends the way an administrator saving the E-mail page does.
type stubResolver struct {
	transport  transport.Transport
	baseURL    string
	configured bool
	calls      int
}

func (s *stubResolver) Transport(context.Context) (transport.Transport, string, bool) {
	s.calls++
	return s.transport, s.baseURL, s.configured
}

func TestResolverIsConsultedOnEverySend(t *testing.T) {
	sink := &recordingTransport{}
	resolver := &stubResolver{transport: transport.NullTransport{}, configured: false}
	composer, err := New(Config{Brand: brandStub{nil}, Resolver: resolver, PublicBaseURL: "https://boot.example"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Before the save: nothing is configured anywhere, so an invitation must
	// report exactly that rather than claim a delivery.
	if composer.Configured(context.Background()) {
		t.Fatal("an unconfigured resolver reported configured")
	}
	if err := composer.SendInvitation(context.Background(), Invitation{Email: "ada@example.com"}); err == nil {
		t.Fatal("an unconfigured deployment reported a delivery")
	}

	// The operator saves a relay. No restart, no new Composer.
	resolver.transport = sink
	resolver.baseURL = "https://saved.example"
	resolver.configured = true

	if !composer.Configured(context.Background()) {
		t.Fatal("a saved relay did not take effect until a restart")
	}
	if err := composer.SendInvitation(context.Background(), Invitation{Email: "ada@example.com"}); err != nil {
		t.Fatalf("SendInvitation: %v", err)
	}
	if len(sink.sent) != 1 {
		t.Fatalf("sent %d messages", len(sink.sent))
	}
	// The LINK comes from the resolution too, not from the origin the process
	// booted with: an operator who corrects the public URL fixes the next
	// invitation, not the next release.
	if !strings.Contains(sink.sent[0].Text, "https://saved.example") {
		t.Errorf("the invitation link did not use the resolved origin:\n%s", sink.sent[0].Text)
	}
	if strings.Contains(sink.sent[0].Text, "https://boot.example") {
		t.Errorf("the invitation carried the boot-time origin:\n%s", sink.sent[0].Text)
	}
	if resolver.calls < 3 {
		t.Errorf("the resolver was asked %d times; it must be asked on every send", resolver.calls)
	}
}

// Shadow mode still outranks a resolved relay: a deployment mirroring traffic
// for comparison must produce no side effect a user can see, and e-mail is the
// first one the security verification specification names.
func TestSuppressionOutranksTheResolver(t *testing.T) {
	sink := &recordingTransport{}
	resolver := &stubResolver{transport: sink, baseURL: "https://saved.example", configured: true}
	composer, _ := New(Config{Resolver: resolver, Suppressed: true})
	if composer.Configured(context.Background()) {
		t.Fatal("a suppressed deployment reported configured")
	}
	if err := composer.SendTest(context.Background(), "ada@example.com"); !errors.Is(err, ErrSuppressed) {
		t.Fatalf("err = %v, want ErrSuppressed", err)
	}
	if len(sink.sent) != 0 {
		t.Fatal("a suppressed deployment dialled the relay")
	}
}
