package storage

import (
	"context"
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
	if got, err := DefaultEncryptionConfirmed(ctx, Instrument(plain{}, "s3")); err != nil || got {
		t.Fatalf("a backend that cannot answer = %t, %v; want not confirmed", got, err)
	}
}
