package repos

// The desktop half of unattended runs at commit (ADR-0045 decisions 3 and 4,
// #1435, #1436): each answered question goes to the approval ledger with its
// source, and the chain's last commit ends the chain.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// desktopTimeoutReason is the ledger reason of a desktop question that took
// its default answer when its wait ended.
const desktopTimeoutReason = "desktop_timeout"

// Exchange kinds that the commit's ledger rows treat on their own.
const (
	// exchangeKindRemoteConfirmation is a remote toolkit confirmation. Main
	// writes its decision row when it consumes the approval
	// (ConsumeConfirmation), so the commit writes none for an approval.
	exchangeKindRemoteConfirmation = "tool_confirmation"
	// exchangeKindContinue is the step limit's "Continue?".
	exchangeKindContinue = "continue"
	// exchangeKindCommandApproval is a local rule's or a hook's ask. In an
	// unattended turn a rule's ask takes its default (source timeout); only
	// a hook's ask is a grant answer (hook_asks).
	exchangeKindCommandApproval = "command_approval"
	// exchangeKindBrowserSubmit is a browser pane submit or upload the
	// grant's browser_submits answered (ADR-0048 decision 8).
	exchangeKindBrowserSubmit = "browser_submit"
)

// grantCoversExchange reports whether the stored grant answers the
// exchange: hook asks need hook_asks, "Continue?" needs continue_step_limit
// and a browser submit needs its host among the grant's browser_submits. No
// other kind is a grant answer.
func grantCoversExchange(grant unattended.Grant, exchange localturn.HITLExchange) bool {
	switch exchange.Kind {
	case exchangeKindCommandApproval:
		return grant.HookAsks
	case exchangeKindContinue:
		return grant.ContinueStepLimit
	case exchangeKindBrowserSubmit:
		return browserSubmitsCover(grant.BrowserSubmits, exchange.Host)
	}
	return false
}

// browserSubmitsCover reports whether host is one the grant's
// browser_submits names. The entries are exact host names (no wildcard is
// stored, ValidateBrowserSubmits refuses one); an entry written `*.name`
// would cover the hosts under name only. Compared ignoring ASCII case and a
// final dot. An absent host is never covered.
func browserSubmitsCover(hosts []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, entry := range hosts {
		entry = strings.ToLower(strings.TrimSuffix(entry, "."))
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			if suffix != "" && strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if entry != "" && entry == host {
			return true
		}
	}
	return false
}

// recordLocalTurnExchanges writes one ledger decision row per exchange whose
// source is not bypass, and the chain's end state when the commit carries
// one. It runs in the commit transaction, once (a replayed commit returns
// before it).
//
// An approved remote confirmation has its row from the consumption: the
// commit writes no second row. A grant answer that the turn's stored grant
// does not cover gets no row and a warning: the commit does not refuse the
// turn's work, and the ledger never shows a grant decision that the grant
// did not give.
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
		if exchange.Kind == exchangeKindRemoteConfirmation && exchangeApproves(exchange) {
			continue
		}
		switch exchange.Source {
		case localturn.SourceBypass:
			// Bypass turns a local rule's ask into allow: no question was
			// asked, so the ledger holds nothing.
			continue
		case localturn.SourceGrant:
			// A grant answer needs the turn's grant, and the grant must
			// cover the exchange's kind.
			if !found || !grantCoversExchange(chain.Grant, exchange) {
				slog.WarnContext(ctx, "local turn: a grant answer that the turn's grant does not cover; no ledger row",
					"execution_id", record.ExecutionID, "interrupt_id", exchange.InterruptID, "kind", exchange.Kind,
					"has_grant", found)
				continue
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

// exchangeApproves reports an exchange that approved its question: the
// grant's answer, or a person's approval.
func exchangeApproves(exchange localturn.HITLExchange) bool {
	switch exchange.Source {
	case localturn.SourceGrant:
		return true
	case "", localturn.SourcePerson:
		return localturn.ExchangeLedgerOutcome(exchange.Decision) == ApprovalOutcomeApproved
	}
	return false
}
