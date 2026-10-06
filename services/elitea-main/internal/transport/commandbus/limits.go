package commandbus

import (
	"errors"
	"math"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

var ErrControlMessageLimitExceeded = errors.New("CONTROL_MESSAGE_LIMIT_EXCEEDED")

type Limits struct {
	Revision                 string
	MaxWorkerCommandBytes    int
	MaxSignedEnvelopeBytes   int
	MaxTransportPayloadBytes int
	MaxTransportMessageBytes int
	MaxSignatureBytes        int
	MaxStringBytes           int
}

func (l Limits) validate() error {
	if l.Revision == "" || l.MaxWorkerCommandBytes <= 0 || l.MaxSignedEnvelopeBytes <= 0 || l.MaxTransportPayloadBytes <= 0 || l.MaxTransportMessageBytes <= 0 || l.MaxSignatureBytes <= 0 || l.MaxStringBytes <= 0 {
		return errors.New("invalid command bus limits")
	}
	if l.MaxWorkerCommandBytes > l.MaxSignedEnvelopeBytes || l.MaxSignedEnvelopeBytes > l.MaxTransportPayloadBytes || l.MaxTransportPayloadBytes > l.MaxTransportMessageBytes || l.MaxTransportMessageBytes > MaxMessageBytes {
		return errors.New("command bus limits are not monotonic")
	}
	return nil
}

func LimitsFromProto(profile *runtimev1.ProtocolLimitsV1) (Limits, error) {
	if profile == nil || profile.GetMaxWorkerCommandBytes() > math.MaxInt || profile.GetMaxSignedEnvelopeBytes() > math.MaxInt || profile.GetMaxTransportPayloadBytes() > math.MaxInt || profile.GetMaxTransportMessageBytes() > math.MaxInt || uint64(profile.GetMaxSafeStringBytes()) > math.MaxInt {
		return Limits{}, errors.New("invalid protocol limits profile")
	}
	limits := Limits{
		Revision:                 profile.GetLimitsRevision(),
		MaxWorkerCommandBytes:    int(profile.GetMaxWorkerCommandBytes()),
		MaxSignedEnvelopeBytes:   int(profile.GetMaxSignedEnvelopeBytes()),
		MaxTransportPayloadBytes: int(profile.GetMaxTransportPayloadBytes()),
		MaxTransportMessageBytes: int(profile.GetMaxTransportMessageBytes()),
		MaxSignatureBytes:        256,
		MaxStringBytes:           int(profile.GetMaxSafeStringBytes()),
	}
	if err := limits.validate(); err != nil {
		return Limits{}, err
	}
	return limits, nil
}
