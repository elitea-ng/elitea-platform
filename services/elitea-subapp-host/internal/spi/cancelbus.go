package spi

import "context"

// CancelBus carries a cancel from the replica it landed on to the replica
// running the invocation (issue #1359).
//
// Invocations are accepted, run and stopped by ONE process: the run's
// goroutine, its engine stream and its stop flag live there. The durable
// store makes an invocation READABLE from every replica (GetInvocation drains
// the shared rows), but a stop is only acted on by the process that checks
// the flag — so a Cancel that lands on another replica is published here, and
// every replica's subscriber stops the run if it is the one running it
// (Manager.StopLocal; anyone else ignores the id).
//
// Delivery is at most once. A lost message leaves the request recorded on
// the row (the store's stop flag) and the run going until it ends; the caller
// sees it run to completion, which is what a cancel that arrives too late
// looks like anyway. internal/cancelbus is the NATS implementation.
type CancelBus interface {
	// Publish announces a cancel of the invocation to every replica.
	Publish(ctx context.Context, invocationID string) error
	// Subscribe calls stop with the id of every cancel announced, until ctx
	// ends.
	Subscribe(ctx context.Context, stop func(invocationID string)) error
}
