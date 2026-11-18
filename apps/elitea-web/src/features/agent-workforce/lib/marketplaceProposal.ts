/**
 * Pure helpers for the Marketplace proposals of workforce managers (ADR-0051
 * decision 6): what the payload of each kind carries, read defensively
 * because an agent wrote part of it.
 */
import type { AgentWorkforceProposal } from '@/shared/api/generated/model';

/** The longest report text (marketplace.MaxReportBody in Main). */
export const REPORT_TEXT_MAX = 500;

type HireMode = 'install' | 'fork';

/** One requirement binding a manager suggested. */
interface BindingHint {
  readonly requirementKey: string;
  readonly toolkitId: string;
}

const text = (value: unknown): string => (typeof value === 'string' ? value : '');
const num = (value: unknown): number | undefined => (typeof value === 'number' && Number.isFinite(value) ? value : undefined);

interface HireProposalView {
  readonly listingId: string;
  readonly versionId: string;
  readonly listingName: string;
  readonly versionName: string;
  readonly mode: HireMode;
  readonly tier: string;
  readonly hints: readonly BindingHint[];
  readonly reviews: number | undefined;
  readonly rating: number | undefined;
  readonly dismissalRate: number | undefined;
  readonly hires: number | undefined;
}

function hintsOf(value: unknown): BindingHint[] {
  if (!Array.isArray(value)) return [];
  return value.flatMap((entry): BindingHint[] => {
    const row = (entry ?? {}) as Record<string, unknown>;
    const id = num(row['toolkit_id']);
    const key = text(row['requirement_key']);
    return key === '' || id === undefined ? [] : [{ requirementKey: key, toolkitId: String(id) }];
  });
}

export function hireView(proposal: AgentWorkforceProposal): HireProposalView {
  const payload = proposal.payload;
  const evidence = proposal.evidence ?? {};
  return {
    listingId: String(proposal.listing_id ?? ''),
    versionId: String(proposal.listing_version_id ?? ''),
    listingName: text(payload['listing_name']),
    versionName: text(payload['version_name']),
    mode: payload['mode'] === 'fork' ? 'fork' : 'install',
    tier: text(payload['tier']),
    hints: hintsOf(payload['bindings_hint']),
    reviews: num(evidence['version_reviews']),
    rating: num(evidence['version_rating_avg']),
    dismissalRate: num(evidence['dismissal_rate']),
    hires: num(evidence['listing_hires']),
  };
}

interface ReportProposalView {
  readonly outcome: string;
  readonly stage: string;
  readonly listingName: string;
  readonly scorecard: readonly (readonly [string, number])[];
}

const SCORECARD_KEYS = ['failure_rate', 'breaker_trips', 'cost_vs_median', 'active_days'] as const;

export function reportView(proposal: AgentWorkforceProposal): ReportProposalView {
  const payload = proposal.payload;
  const card = (payload['scorecard'] ?? {}) as Record<string, unknown>;
  return {
    outcome: text(payload['outcome']),
    stage: proposal.stage,
    listingName: text(payload['listing_name']),
    scorecard: SCORECARD_KEYS.flatMap((key): [string, number][] => {
      const value = num(card[key]);
      return value === undefined ? [] : [[key, value]];
    }),
  };
}
