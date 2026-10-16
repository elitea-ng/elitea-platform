package storage

import (
	"context"
	"errors"
	"testing"
)

type encryptedStub struct {
	ObjectStore
	confirmed bool
}

func (s encryptedStub) DefaultEncryptionConfirmed(context.Context) (bool, error) {
	return s.confirmed, nil
}

func TestDefaultEncryptionConfirmedThroughTheInstrumentedWrapper(t *testing.T) {
	ctx := context.Background()
	for _, want := range []bool{true, false} {
		got, err := DefaultEncryptionConfirmed(ctx, Instrument(encryptedStub{confirmed: want}, "s3"))
		if err != nil || got != want {
			t.Fatalf("wrapped verifier = %t, %v; want %t", got, err, want)
		}
	}
	type plain struct{ ObjectStore }
	if got, err := DefaultEncryptionConfirmed(ctx, Instrument(plain{}, "s3")); !errors.Is(err, ErrEncryptionUnreportable) || got {
		t.Fatalf("a backend that cannot answer = %t, %v; want not confirmed and unreportable", got, err)
	}
}

type lifecycleStub struct {
	ObjectStore
	calls int
}

func (s *lifecycleStub) ConfigureRetentionLifecycle(context.Context) error {
	s.calls++
	return nil
}

// The startup lifecycle setup reaches the backend through the instrumented
// wrapper (it used to stop at the wrapper, which had no such method).
func TestConfigureRetentionLifecycleThroughTheInstrumentedWrapper(t *testing.T) {
	inner := &lifecycleStub{}
	wrapped := Instrument(inner, "s3")
	configurer, ok := wrapped.(RetentionLifecycleConfigurer)
	if !ok {
		t.Fatalf("the instrumented store hides ConfigureRetentionLifecycle")
	}
	if err := configurer.ConfigureRetentionLifecycle(context.Background()); err != nil || inner.calls != 1 {
		t.Fatalf("calls = %d, %v", inner.calls, err)
	}
	type plain struct{ ObjectStore }
	if err := Instrument(plain{}, "azure").(RetentionLifecycleConfigurer).ConfigureRetentionLifecycle(context.Background()); err != nil {
		t.Fatalf("a backend without the rule = %v", err)
	}
}
