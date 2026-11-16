import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError, unwrap } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';

/**
 * Workforce managers on the Marketplace (ADR-0051 decision 6, client contract 1.56:
 * listAgentWorkforceProposals, getAgentWorkforce). The app only reads: the open hire proposals and
 * drafted reports of the project (an agent manager's; others are refused with 403), and whether an
 * agent the workforce policy hired is still on probation. Hiring, publishing and discarding are on
 * the web and desktop.
 *
 * An agent wrote the rationale and the draft: it is shown as plain text and never interpreted.
 */

/** Client contract 1.56 tags both routes `client`. */
export const WORKFORCE_CONTRACT = { major: 1, minor: 56 } as const;

const MAX_TEXT = 4200;

const Proposal = z.looseObject({
  id: z.number().int().positive(),
  kind: z.string().max(40),
  status: z.string().max(20).catch(''),
  stage: z.string().max(20).catch(''),
  rationale: z.string().max(MAX_TEXT).catch(''),
  payload: z.record(z.string(), z.unknown()).catch({}),
});

const Proposals = z.looseObject({ rows: z.array(z.unknown()).max(100) });

export interface MarketplaceProposalView {
  id: number;
  kind: 'hire_listing' | 'feedback_report';
  /** The listing's name, as the proposal recorded it. */
  listingName: string;
  /** A hire's version and mode; empty for a report. */
  versionName: string;
  mode: 'install' | 'fork' | null;
  /** A report's outcome and ladder stage; empty for a hire. */
  outcome: string;
  stage: string;
  /** What the manager wrote (the reason for a hire, the draft of a report). */
  text: string;
}

const text = (value: unknown, max = 200): string => (typeof value === 'string' ? value.trim().slice(0, max) : '');

/** The open Marketplace proposals of an answer: hires and reports, nothing else. */
export function marketplaceProposalsOf(data: unknown): MarketplaceProposalView[] {
  const parsed = Proposals.safeParse(data);
  if (!parsed.success) return [];
  const out: MarketplaceProposalView[] = [];
  for (const raw of parsed.data.rows) {
    const row = Proposal.safeParse(raw);
    if (!row.success) continue;
    if (row.data.kind !== 'hire_listing' && row.data.kind !== 'feedback_report') continue;
    if (row.data.status !== 'proposed' && row.data.status !== 'asked_owner') continue;
    const payload = row.data.payload;
    out.push({
      id: row.data.id,
      kind: row.data.kind,
      listingName: text(payload.listing_name, 120),
      versionName: text(payload.version_name, 80),
      mode: payload.mode === 'fork' ? 'fork' : payload.mode === 'install' ? 'install' : null,
      outcome: text(payload.outcome, 20),
      stage: row.data.stage,
      text: row.data.rationale.trim(),
    });
  }
  return out;
}

/** "Hire PR Reader v2.3 (install)" or "Report on PR Reader: dismissed at retire". */
export function proposalTitle(proposal: MarketplaceProposalView): string {
  const name = proposal.listingName || 'a listing';
  if (proposal.kind === 'hire_listing') {
    const version = proposal.versionName ? ` ${proposal.versionName}` : '';
    const mode = proposal.mode ? ` (${proposal.mode})` : '';
    return `Hire ${name}${version}${mode}`;
  }
  const outcome = proposal.outcome ? `: ${proposal.outcome}` : '';
  const stage = proposal.stage ? ` at ${proposal.stage}` : '';
  return `Report on ${name}${outcome}${stage}`;
}

/** What the screen says when the server refuses the read: only an agent manager sees these. */
export function proposalsErrorText(error: unknown): string {
  if (isApiError(error) && error.status === 403) return 'Only an agent manager sees the workforce proposals of this project.';
  if (isApiError(error) && error.kind === 'network') return 'You appear to be offline.';
  return 'The proposals could not be loaded.';
}

export async function fetchMarketplaceProposals(projectId: number, client: ApiClient = sessionApiClient(), signal?: AbortSignal): Promise<MarketplaceProposalView[]> {
  const data = await unwrap(
    client.GET('/elitea_core/agent_workforce/prompt_lib/{project_id}/proposals', {
      params: { path: { project_id: String(projectId) }, query: { kind: 'marketplace', limit: 50 } },
      signal,
    }),
  );
  return marketplaceProposalsOf(data);
}

export function useMarketplaceProposals(projectId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'workforce-proposals'],
    enabled: enabled && projectId !== null,
    staleTime: 60_000,
    retry: false,
    queryFn: ({ signal }) => fetchMarketplaceProposals(projectId as number, sessionApiClient(), signal),
  });
}

const Probation = z.looseObject({
  auto_hired_at: z.string().max(40).nullish().catch(null),
  probation_until: z.string().max(40).nullish().catch(null),
  probation_reviewed_at: z.string().max(40).nullish().catch(null),
  lifecycle_state: z.string().max(20).catch('active'),
});

/** The probation of an agent: its end, or null when it was not hired automatically or a manager kept it. */
export function probationOf(data: unknown): { until: string | null } | null {
  const parsed = Probation.safeParse(data);
  if (!parsed.success) return null;
  const agent = parsed.data;
  if (!agent.auto_hired_at || agent.probation_reviewed_at || agent.lifecycle_state !== 'active') return null;
  return { until: agent.probation_until ?? null };
}

export async function fetchAgentProbation(projectId: number, agentId: number, client: ApiClient = sessionApiClient(), signal?: AbortSignal) {
  const data = await unwrap(
    client.GET('/elitea_core/agent_workforce/prompt_lib/{project_id}/agents/{application_id}', {
      params: { path: { project_id: String(projectId), application_id: agentId } },
      signal,
    }),
  );
  return probationOf(data);
}

export function useAgentProbation(projectId: number | null, agentId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'workforce-agent', agentId ?? 0],
    enabled: enabled && projectId !== null && agentId !== null,
    staleTime: 2 * 60_000,
    retry: false,
    queryFn: ({ signal }) => fetchAgentProbation(projectId as number, agentId as number, sessionApiClient(), signal),
  });
}
