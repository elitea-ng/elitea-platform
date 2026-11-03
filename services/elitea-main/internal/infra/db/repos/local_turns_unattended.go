package repos

// The desktop half of unattended runs at commit (ADR-0045 decisions 3 and 4,
// #1435, #1436): each answered question goes to the approval ledger with its
// source, and the chain's last commit ends the chain.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// desktopTimeoutReason is the ledger reason of a desktop question that took
// its default answer when its wait ended.
const desktopTimeoutReason = "desktop_timeout"

// recordLocalTurnExchanges writes one ledger decision row per exchange whose
// source is not bypass, and the chain's end state when the commit carries
// one. It runs in the commit transaction, once (a replayed commit returns
// before it).
func recordLocalTurnExchanges(
	ctx context.Context, tx pgx.Tx, record localturn.CommitRecord, scope localTurnLedgerScope,
) error {
	needsChain := record.ChainOutcome != ""
	for _, exchange := range record.Exchanges {
		if exchange.Source == localturn.SourceGrant {
			needsChain = true
		}
	}
	var (
		chain unattended.Chain
		found bool
		err   error
	)
	if needsChain {
		if chain, found, err = readRunChainLink(ctx, tx, record.ExecutionID); err != nil {
			return err
		}
	}
	for _, exchange := range record.Exchanges {
		entry := approvalEntry{
			ProjectID: scope.projectID, ExecutionID: record.ExecutionID, ResponseMessageID: scope.responseMessageID,
			ConversationUUID: scope.conversationUUID, InterruptID: exchange.InterruptID, Path: approvalPathDesktop,
			Phase: approvalPhaseDecision, TriggerOrigin: "manual",
			Call: approvalCall{GuardrailType: exchange.Kind},
		}
		switch exchange.Source {
		case localturn.SourceBypass:
			// Bypass turns a local rule's ask into allow: no question was
			// asked, so the ledger holds nothing.
			continue
		case localturn.SourceGrant:
			// A grant answer needs the turn's grant: a turn without one
			// cannot claim one.
			if !found {
				return fmt.Errorf("%w: a grant answer on a turn without a grant", localturn.ErrInvalid)
			}
			entry.Outcome, entry.Action = ApprovalOutcomeApproved, localturn.ExchangeLedgerAction(exchange.Decision)
			entry.ActorUserID, entry.Reason, entry.GrantID = chain.OwnerUserID, unattended.ReasonUnattendedGrant, chain.GrantID
		case localturn.SourceTimeout:
			entry.Outcome, entry.Reason = ApprovalOutcomeTimedOut, desktopTimeoutReason
		default:
			actor, _ := strconv.ParseInt(scope.actorID, 10, 64)
			entry.Outcome = localturn.ExchangeLedgerOutcome(exchange.Decision)
			entry.Action, entry.ActorUserID = localturn.ExchangeLedgerAction(exchange.Decision), actor
		}
		if _, err := recordApproval(ctx, pgxExecutor{queryer: tx}, entry); err != nil {
			return fmt.Errorf("local turn: record the exchange: %w", err)
		}
	}
	if record.ChainOutcome != "" && found {
		if _, err := endRunChain(ctx, tx, chain.RootExecutionID, record.ChainOutcome, ""); err != nil {
			return err
		}
	}
	return nil
}
