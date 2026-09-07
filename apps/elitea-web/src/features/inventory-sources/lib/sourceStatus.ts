/**
 * How one source's ingestion status is shown.
 *
 * THE VOCABULARY IS THE LEGACY ONE, kept verbatim rather than tidied: the
 * plugin's own card wrote `pending → "Pending"`, `in_progress → "Ingesting…"`,
 * `completed → "Done"`, `error → "Error"`, and carried three more spellings
 * (`ingesting`, `ingested`, `done`) "for backwards compatibility" — rows
 * written by an older ingestion that are still in a live `sources_status.json`.
 * Dropping them would render a completed source as unknown.
 *
 * AN UNREPORTED STATUS IS NOT AN ERROR. A source that was added and never
 * ingested has no row in the status document at all; it is waiting, and the
 * screen says so. Showing it as an error would report a failure where nothing
 * has happened yet.
 *
 * THE LABEL IS NOT RESOLVED HERE. This module answers a KIND, and the
 * component turns the kind into copy with a literal `t(key, fallback)` call.
 * Returning the key from here would put every one of these strings beyond
 * `scripts/i18n-backfill.mjs`, which only extracts literal call sites — the
 * copy would work and no translator would ever see it.
 */

/** The colour vocabulary the chip renders in — MUI palette names, never a hex. */
type SourceStatusTone = 'default' | 'info' | 'success' | 'warning' | 'error';

/** The five readings this table has. */
export type SourceStatusKind = 'waiting' | 'pending' | 'ingesting' | 'done' | 'error';

/** One status, resolved into what the screen needs. */
export interface SourceStatusView {
  readonly kind: SourceStatusKind;
  readonly tone: SourceStatusTone;
  /** Whether this status means an ingestion is under way for this source. */
  readonly busy: boolean;
}

const STATUSES: Readonly<Record<string, SourceStatusView>> = {
  pending: { kind: 'pending', tone: 'warning', busy: false },
  in_progress: { kind: 'ingesting', tone: 'info', busy: true },
  ingesting: { kind: 'ingesting', tone: 'info', busy: true },
  completed: { kind: 'done', tone: 'success', busy: false },
  ingested: { kind: 'done', tone: 'success', busy: false },
  done: { kind: 'done', tone: 'success', busy: false },
  error: { kind: 'error', tone: 'error', busy: false },
};

const NOT_INGESTED: SourceStatusView = { kind: 'waiting', tone: 'default', busy: false };

/**
 * What to show for one reported status.
 *
 * `null` means the provider reported something this table has no reading for.
 * The caller prints that text as it stands, because a status invented by a
 * newer engine is information; folding it into "Not ingested" would tell the
 * user the opposite of what the provider said.
 */
export function sourceStatusView(status: string): SourceStatusView | null {
  const key = status.trim().toLowerCase();
  if (key === '') return NOT_INGESTED;
  return STATUSES[key] ?? null;
}
