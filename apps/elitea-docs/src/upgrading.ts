/**
 * The upgrade notes in page order, shared by the page body
 * (src/components/UpgradeNotes.astro) and its table of contents
 * (src/route-middleware.ts), so the two cannot disagree.
 *
 * Order: `date`, newest first. Within one date the entries without `order`
 * come first, by `issue` (highest first) and then file name; then the
 * entries with `order` (the ones moved from the old single file), highest
 * first, which keeps their old order. docs/upgrading/README.md states the
 * same rule for contributors.
 */
import { getCollection, type CollectionEntry } from 'astro:content';

export const UPGRADING_PAGE = 'deployment/upgrading';

export type UpgradeNote = CollectionEntry<'upgrading'>;

export async function upgradeNotes(): Promise<UpgradeNote[]> {
  const last = Number.POSITIVE_INFINITY;
  return (await getCollection('upgrading')).sort(
    (a, b) =>
      b.data.date.getTime() - a.data.date.getTime() ||
      (b.data.order ?? last) - (a.data.order ?? last) ||
      b.data.issue - a.data.issue ||
      (a.id < b.id ? -1 : a.id > b.id ? 1 : 0),
  );
}

/** A title's text as a reader sees it (backticks dropped). */
export function plainTitle(title: string): string {
  return title.replaceAll('`', '');
}
