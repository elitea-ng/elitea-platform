package runtimecomposition

import (
	"context"

	"github.com/nats-io/nats.go"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

const (
	securityInvalidationPendingMessages = 1024
	securityInvalidationPendingBytes    = 1 << 20
)

// natsSecurityInvalidationTransport carries the governed-credential
// invalidation fan-out on core NATS (commandbus.SecurityInvalidationSubject,
// the elitea-main-runtime identity, which both publishes and subscribes).
type natsSecurityInvalidationTransport struct{ conn *nats.Conn }

func (t natsSecurityInvalidationTransport) Publish(data []byte) error {
	return t.conn.Publish(commandbus.SecurityInvalidationSubject, data)
}

func (t natsSecurityInvalidationTransport) Subscribe(deliver func([]byte)) (func() error, error) {
	sub, err := t.conn.Subscribe(commandbus.SecurityInvalidationSubject, func(msg *nats.Msg) { deliver(msg.Data) })
	if err != nil {
		return nil, err
	}
	if err := sub.SetPendingLimits(securityInvalidationPendingMessages, securityInvalidationPendingBytes); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	return sub.Unsubscribe, nil
}

// securityInvalidationRunner runs the hub over NATS as a runtime publisher.
type securityInvalidationRunner struct {
	hub       *epoch.Hub
	transport epoch.Transport
}

func (r securityInvalidationRunner) Run(ctx context.Context) error {
	return r.hub.Run(ctx, r.transport)
}
