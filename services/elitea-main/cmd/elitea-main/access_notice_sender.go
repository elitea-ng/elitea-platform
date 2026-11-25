package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// startAccessNoticeSender runs the sender of the access notices (ADR-0059
// R2.5a, adminaccess.NoticeSender) on a routines.Loop: one replica per pass,
// every 30 seconds by default, changeable on the admin Schedules page. Each
// row is also claimed under FOR UPDATE SKIP LOCKED, so the routine registry's
// local fallback during an outage cannot send a row twice at once. It runs in
// maintenance too: a break-glass notice is not deferred work. With no mailer
// configured it sends nothing and logs how many notices wait; nothing is
// dropped. The loop stops when ctx ends.
//
// origin is the deployment's public origin (publicOriginFromEnv), the base of
// the approval link of a request notice.
func startAccessNoticeSender(ctx context.Context, pool *pgxpool.Pool, mailer adminaccess.NoticeMailer,
	origin string, logger *slog.Logger,
) error {
	if pool == nil {
		return nil
	}
	sender, err := adminaccess.NewNoticeSender(pool, mailer, origin, time.Now)
	if err != nil {
		return fmt.Errorf("compose the access notice sender: %w", err)
	}
	webhook, err := noticeWebhookFromEnv(os.Getenv, logger)
	if err != nil {
		return err
	}
	if webhook != nil {
		sender.WithWebhook(webhook)
	}
	runAccessNoticeSender(ctx, routines.NewStore(pool), sender.Pass, logger)
	return nil
}

// noticeWebhookFromEnv reads the out-of-band notice webhook (ADR-0059
// R2.5b) and, when it is set, turns on its rows in the outbox: it runs
// before the router is built, so every event written afterwards has its
// webhook row. Unset, it answers nil and the outbox stays e-mail only. A URL
// that is not https, a weak signing secret, a secret or an allowlist without
// a URL, or a malformed allowlist refuses the start: a deployment that meant
// to configure the webhook never runs silently without it. No error names
// the URL.
func noticeWebhookFromEnv(getenv func(string) string, logger *slog.Logger) (adminaccess.NoticeWebhookSender, error) {
	config, ok, err := adminaccess.NoticeWebhookConfigFromEnv(getenv)
	if err != nil {
		return nil, fmt.Errorf("break-glass notice webhook: %w", err)
	}
	if !ok {
		adminaccess.SetNoticeWebhookEnabled(false)
		return nil, nil
	}
	webhook, err := adminaccess.NewNoticeWebhook(config, logger)
	if err != nil {
		return nil, fmt.Errorf("break-glass notice webhook: %w", err)
	}
	adminaccess.SetNoticeWebhookEnabled(true)
	logger.Info("break-glass notice webhook configured")
	return webhook, nil
}

// accessNoticeWaitWarnEvery spaces the "notices wait for a mailer" warning.
const accessNoticeWaitWarnEvery = 15 * time.Minute

// runAccessNoticeSender runs pass on the routine's loop until ctx ends; the
// returned channel closes when the loop has stopped.
func runAccessNoticeSender(ctx context.Context, backend routines.Backend,
	pass func(context.Context) (adminaccess.NoticeSendResult, error), logger *slog.Logger,
) <-chan struct{} {
	loop := &routines.Loop{
		Backend: backend,
		Def: routines.Definition{
			ID:              adminaccess.AccessNoticeSenderRoutineID,
			Owner:           routines.OwnerMain,
			// The text is shown on the Schedules page and held by its
			// visual baseline (admin-schedules). It does not name the
			// out-of-band webhook (#2598), which the same pass delivers.
			Description:     "Sends the break-glass access notices and the notice to owners of inherit tokens by e-mail, and retries the ones the mailer refused.",
			DefaultSchedule: adminaccess.AccessNoticeSenderDefaultSchedule,
			MinInterval:     10 * time.Second,
		},
		Logger: logger,
	}
	done := make(chan struct{})
	// The "no mailer" warning is logged when the waiting starts and then at
	// most every accessNoticeWaitWarnEvery, not on every 30-second pass.
	var lastWaitWarn, lastWebhookWarn time.Time
	go func() {
		defer close(done)
		loop.Run(ctx, func(ctx context.Context) (string, error) {
			result, err := pass(ctx)
			waiting := !result.MailerConfigured && result.Pending > 0
			if !waiting {
				lastWaitWarn = time.Time{}
			}
			switch {
			case waiting:
				if lastWaitWarn.IsZero() || time.Since(lastWaitWarn) >= accessNoticeWaitWarnEvery {
					lastWaitWarn = time.Now()
					logger.Warn("access notices wait for a mailer: configure outbound e-mail",
						"pending", result.Pending)
				}
			case result.Sent > 0 || result.Failed > 0:
				logger.Info("access notice sender pass",
					"sent", result.Sent, "failed", result.Failed, "pending", result.Pending)
			}
			if !result.WebhookConfigured && result.WebhookPending > 0 {
				if lastWebhookWarn.IsZero() || time.Since(lastWebhookWarn) >= accessNoticeWaitWarnEvery {
					lastWebhookWarn = time.Now()
					logger.Warn("break-glass webhook notices wait for a webhook: the notice webhook is no longer configured",
						"pending", result.WebhookPending)
				}
			} else {
				lastWebhookWarn = time.Time{}
			}
			if result.WebhookSent > 0 || result.WebhookFailed > 0 {
				logger.Info("access notice webhook pass", "sent", result.WebhookSent,
					"failed", result.WebhookFailed, "pending", result.WebhookPending)
			}
			if err != nil {
				logger.Error("access notice sender pass failed", "err", err)
				return routines.OutcomeError, err
			}
			return routines.OutcomeOK, nil
		})
	}()
	return done
}
