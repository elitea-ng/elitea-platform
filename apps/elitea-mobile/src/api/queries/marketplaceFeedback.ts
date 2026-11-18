import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';

/**
 * The feedback of a Marketplace listing (ADR-0051 decision 5, client contract 1.55:
 * searchMarketplaceListings, getMarketplaceFeedback). The app only reads it, on the agent sheet
 * of Discover: the rating as text, the signals beside it and the newest reviews and workforce
 * reports. Writing a review, replying and moderating are on the web and desktop listing page.
 *
 * The public agent id (`entity_id` of the listing) finds the listing; the project in the path is
 * the active one. A review shows its author's name, a workforce report only "from a hiring
 * project": the server never sends a project name, and nothing here would show one.
 */

/** Client contract 1.55 tags both routes `client`. */
export const MARKETPLACE_FEEDBACK_CONTRACT = { major: 1, minor: 55 } as const;

/** How many items the sheet shows. */
export const FEEDBACK_ITEMS = 5;
const MAX_TEXT = 2000;

const Count = z.number().int().nonnegative();
const Rate = z.number().min(0).max(1).nullable().catch(null);

const Item = z.looseObject({
  id: z.string().min(1).max(30),
  kind: z.string().max(40),
  version_name: z.string().max(200).catch(''),
  rating: z.number().int().min(1).max(5).nullable().catch(null),
  body: z.string().max(MAX_TEXT).catch(''),
  outcome: z.string().max(40).nullish().catch(null),
  author: z.looseObject({ name: z.string().max(300).catch('') }).catch({ name: '' }),
  state: z.string().max(20).catch('visible'),
});

const Feedback = z.looseObject({
  items: z.array(z.unknown()).max(100),
  total: Count.catch(0),
  overall: z
    .looseObject({ rating_avg: z.number().min(0).max(5).nullable().catch(null), rating_count: Count.catch(0) })
    .catch({ rating_avg: null, rating_count: 0 }),
  signals: z
    .looseObject({
      listing: z
        .looseObject({
          active_hires: Count.catch(0),
          retention_30d: Rate,
          dismissal_rate: Rate,
          upgrade_adoption: Rate,
        })
        .catch({ active_hires: 0, retention_30d: null, dismissal_rate: null, upgrade_adoption: null }),
    })
    .catch({ listing: { active_hires: 0, retention_30d: null, dismissal_rate: null, upgrade_adoption: null } }),
});

const Listings = z.looseObject({ rows: z.array(z.looseObject({ id: z.string().regex(/^\d{1,15}$/) })).max(5) });

export interface FeedbackItemView {
  id: string;
  isReport: boolean;
  /** The person's name for a review; the server's "from a hiring project" label for a report. */
  author: string;
  versionName: string;
  /** 1 to 5 for a review; null for a report. */
  rating: number | null;
  /** A report's outcome (`kept`, `demoted`, `dismissed`), else null. */
  outcome: string | null;
  body: string;
}

export interface ListingFeedbackView {
  /** Reviews only. Null when there are none. */
  ratingAvg: number | null;
  ratingCount: number;
  activeHires: number;
  /** Whole percentages, or null for a rate that was never measured. */
  retentionPercent: number | null;
  dismissalPercent: number | null;
  upgradePercent: number | null;
  items: FeedbackItemView[];
  total: number;
}

function percent(rate: number | null): number | null {
  return rate === null ? null : Math.round(rate * 100);
}

/** The listing id of the first row, or null when the agent has no listing. */
export function listingIdOf(data: unknown): string | null {
  const parsed = Listings.safeParse(data);
  return parsed.success ? (parsed.data.rows[0]?.id ?? null) : null;
}

/** One feedback answer as the sheet shows it. Hidden items and unknown kinds are dropped. */
export function listingFeedbackOf(data: unknown): ListingFeedbackView | null {
  const parsed = Feedback.safeParse(data);
  if (!parsed.success) return null;
  const items: FeedbackItemView[] = [];
  for (const raw of parsed.data.items) {
    const item = Item.safeParse(raw);
    if (!item.success || item.data.state !== 'visible') continue;
    if (item.data.kind !== 'review' && item.data.kind !== 'workforce_report') continue;
    items.push({
      id: item.data.id,
      isReport: item.data.kind === 'workforce_report',
      author: item.data.author.name.trim(),
      versionName: item.data.version_name.trim(),
      rating: item.data.rating,
      outcome: item.data.outcome ? item.data.outcome : null,
      body: item.data.body.trim(),
    });
    if (items.length >= FEEDBACK_ITEMS) break;
  }
  const { overall, signals } = parsed.data;
  return {
    ratingAvg: overall.rating_avg,
    ratingCount: overall.rating_count,
    activeHires: signals.listing.active_hires,
    retentionPercent: percent(signals.listing.retention_30d),
    dismissalPercent: percent(signals.listing.dismissal_rate),
    upgradePercent: percent(signals.listing.upgrade_adoption),
    items,
    total: parsed.data.total,
  };
}

/** The rating as text, never as stars or colour alone. */
export function ratingSentence(view: Pick<ListingFeedbackView, 'ratingAvg' | 'ratingCount'>): string {
  if (view.ratingAvg === null || view.ratingCount === 0) return 'No reviews yet';
  return `Rated ${view.ratingAvg.toFixed(1)} out of 5 by ${view.ratingCount} ${view.ratingCount === 1 ? 'review' : 'reviews'}`;
}

/** "Active hires 12 · 30-day retention 80% · Dismissal rate 20%", leaving out what was never measured. */
export function signalsSentence(view: ListingFeedbackView): string {
  const parts = [`Active hires ${view.activeHires}`];
  if (view.retentionPercent !== null) parts.push(`30-day retention ${view.retentionPercent}%`);
  if (view.dismissalPercent !== null) parts.push(`Dismissal rate ${view.dismissalPercent}%`);
  if (view.upgradePercent !== null) parts.push(`Upgrade adoption ${view.upgradePercent}%`);
  return parts.join(' · ');
}

/** The feedback of the listing a public agent belongs to, or null when it has no listing. */
export async function fetchAgentFeedback(
  projectId: number,
  agentId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<ListingFeedbackView | null> {
  const listings = await unwrap(
    client.GET('/elitea_core/marketplace_listings/prompt_lib/{project_id}', {
      params: { path: { project_id: String(projectId) }, query: { entity_id: String(agentId), kind: 'agent', limit: 1 } },
      signal,
    }),
  );
  const listingId = listingIdOf(listings);
  if (listingId === null) return null;
  const feedback = await unwrap(
    client.GET('/elitea_core/marketplace_feedback/prompt_lib/{project_id}/{listing_id}', {
      params: { path: { project_id: String(projectId), listing_id: listingId }, query: { limit: FEEDBACK_ITEMS } },
      signal,
    }),
  );
  return listingFeedbackOf(feedback);
}

export function useAgentFeedback(projectId: number | null, agentId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.discover(projectId ?? 0), 'feedback', agentId ?? 0],
    enabled: enabled && projectId !== null && agentId !== null,
    staleTime: 2 * 60_000,
    retry: false,
    queryFn: ({ signal }) => fetchAgentFeedback(projectId as number, agentId as number, sessionApiClient(), signal),
  });
}
