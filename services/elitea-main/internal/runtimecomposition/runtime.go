package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/redis/go-redis/v9"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

type publisherRunner interface {
	Run(context.Context) error
}

type privateServerRunner interface {
	Serve(context.Context) error
	Shutdown(context.Context) error
}

// Runtime owns only the new runtime's control Redis client and background
// components. The caller owns Run's context, waits for Run to drain, and calls
// Close after Run returns.
type Runtime struct {
	publisher              publisherRunner
	private                privateServerRunner
	controlRedis           *redis.Client
	sandboxSpoolDir        string
	publicRoutes           PublicRoutes
	configurationValidator configurationapp.CurrentSDKConfigurationValidator
	closeOnce              sync.Once
	closeErr               error
	lifecycleMu            sync.Mutex
	started                bool
	stopped                bool
	shutdown               bool
	startReady             chan struct{}
	publisherEnd           context.CancelFunc
	privateEnd             context.CancelFunc
	runDone                chan struct{}
}

func (r *Runtime) PublicRoutes() PublicRoutes {
	return r.publicRoutes
}

func (r *Runtime) CurrentSDKConfigurationValidator() configurationapp.CurrentSDKConfigurationValidator {
	if r == nil {
		return nil
	}
	return r.configurationValidator
}

func (r *Runtime) Run(ctx context.Context) error {
	if ctx == nil || r.publisher == nil || r.private == nil {
		return errors.New("runtime lifecycle dependencies are incomplete")
	}
	publisherCtx, publisherEnd := context.WithCancel(context.Background())
	privateCtx, privateEnd := context.WithCancel(context.Background())
	r.lifecycleMu.Lock()
	if r.startReady == nil {
		r.startReady = make(chan struct{})
	}
	if r.started {
		r.lifecycleMu.Unlock()
		publisherEnd()
		privateEnd()
		return errors.New("runtime lifecycle already started")
	}
	r.started = true
	close(r.startReady)
	r.publisherEnd = publisherEnd
	r.privateEnd = privateEnd
	r.runDone = make(chan struct{})
	r.lifecycleMu.Unlock()
	defer func() {
		publisherEnd()
		privateEnd()
		r.lifecycleMu.Lock()
		r.stopped = true
		close(r.runDone)
		r.lifecycleMu.Unlock()
	}()
	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	go func() { results <- result{name: "outbox publisher", err: r.publisher.Run(publisherCtx)} }()
	go func() { results <- result{name: "private servers", err: r.private.Serve(privateCtx)} }()

	triggeredByContext := false
	collected := make([]result, 0, 2)
	select {
	case first := <-results:
		collected = append(collected, first)
	case <-ctx.Done():
		triggeredByContext = true
	}
	r.lifecycleMu.Lock()
	triggeredByShutdown := r.shutdown
	r.lifecycleMu.Unlock()
	publisherEnd()
	privateEnd()
	for len(collected) < 2 {
		collected = append(collected, <-results)
	}

	causes := make([]error, 0, 2)
	for _, result := range collected {
		if result.err == nil {
			// A graceful private-server stop legitimately returns nil. Accept it
			// only when the owning context or Runtime.Shutdown initiated this
			// drain; a component that stops nil on its own remains a failure.
			if triggeredByContext || triggeredByShutdown {
				continue
			}
			causes = append(causes, fmt.Errorf("runtime %s stopped unexpectedly", result.name))
			continue
		}
		if errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) {
			continue
		}
		causes = append(causes, fmt.Errorf("runtime %s: %w", result.name, result.err))
	}
	if len(causes) != 0 {
		return errors.Join(causes...)
	}
	r.lifecycleMu.Lock()
	shutdownRequested := r.shutdown
	r.lifecycleMu.Unlock()
	if shutdownRequested {
		return context.Canceled
	}
	if triggeredByContext || ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("runtime component stopped without an owning cancellation")
}

// Shutdown stops publication immediately, drains private listeners through the
// caller-owned deadline, then waits until Run has released every runtime
// goroutine. Returning from this method means Redis and database dependencies
// may be closed safely.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("runtime shutdown context is required")
	}
	r.lifecycleMu.Lock()
	if !r.started {
		if r.startReady == nil {
			r.startReady = make(chan struct{})
		}
		startReady := r.startReady
		r.lifecycleMu.Unlock()
		select {
		case <-startReady:
		case <-ctx.Done():
			return ctx.Err()
		}
		r.lifecycleMu.Lock()
	}
	if r.stopped {
		r.lifecycleMu.Unlock()
		return nil
	}
	r.shutdown = true
	publisherEnd := r.publisherEnd
	privateEnd := r.privateEnd
	runDone := r.runDone
	r.lifecycleMu.Unlock()

	publisherEnd()
	privateErr := r.private.Shutdown(ctx)
	privateEnd()
	select {
	case <-runDone:
		return privateErr
	case <-ctx.Done():
		// The private runner is required to hard-stop at this same deadline.
		// Wait for Run to publish its terminal ownership signal before callers
		// close the pools. A runner violating that contract is a programming bug,
		// not a reason to introduce a use-after-close race.
		<-runDone
		return errors.Join(privateErr, ctx.Err())
	}
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.controlRedis != nil {
			r.closeErr = r.controlRedis.Close()
		}
		if r.sandboxSpoolDir != "" {
			r.closeErr = errors.Join(r.closeErr, os.RemoveAll(r.sandboxSpoolDir))
		}
	})
	return r.closeErr
}
