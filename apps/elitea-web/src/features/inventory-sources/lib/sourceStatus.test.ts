/**
 * How one source's ingestion status is read.
 *
 * The vocabulary is the LEGACY one, and three of its seven spellings —
 * `ingesting`, `ingested`, `done` — exist only because rows written by an older
 * ingestion are still in live `sources_status.json` files. Drop them and a
 * completed source renders as unknown.
 *
 * Two readings are opposite facts and easy to confuse. An UNREPORTED status is
 * a source that was added and never ingested: it is waiting, not failing. A
 * status this table does NOT know is information from a newer engine, and
 * `null` is what tells the chip to print it verbatim — folding it into
 * "Not ingested" tells the user the opposite of what the provider said.
 */
import { describe, expect, it } from 'vitest';

import { sourceStatusView } from './sourceStatus';

describe('sourceStatusView', () => {
  it('reads the four statuses the provider writes today', () => {
    expect(sourceStatusView('pending')).toEqual({ kind: 'pending', tone: 'warning', busy: false });
    expect(sourceStatusView('in_progress')).toEqual({ kind: 'ingesting', tone: 'info', busy: true });
    expect(sourceStatusView('completed')).toEqual({ kind: 'done', tone: 'success', busy: false });
    expect(sourceStatusView('error')).toEqual({ kind: 'error', tone: 'error', busy: false });
  });

  it('reads the three legacy spellings still sitting in live status files', () => {
    expect(sourceStatusView('ingesting')?.kind).toBe('ingesting');
    expect(sourceStatusView('ingested')?.kind).toBe('done');
    expect(sourceStatusView('done')?.kind).toBe('done');
  });

  it('marks only an in-flight ingestion as busy', () => {
    expect(sourceStatusView('ingesting')?.busy).toBe(true);
    expect(sourceStatusView('pending')?.busy).toBe(false);
    expect(sourceStatusView('done')?.busy).toBe(false);
  });

  it('reads an unreported status as WAITING, not as a failure', () => {
    // A source added and never ingested has no row in the status document at
    // all. Showing an error reports a failure where nothing has happened yet.
    expect(sourceStatusView('')).toEqual({ kind: 'waiting', tone: 'default', busy: false });
    expect(sourceStatusView('   ')?.kind).toBe('waiting');
  });

  it('answers null for a word it has no reading for', () => {
    // The chip prints it as it stands. A newer engine's status is information.
    expect(sourceStatusView('quarantined')).toBeNull();
  });

  it('ignores case and surrounding space', () => {
    expect(sourceStatusView(' Completed ')?.kind).toBe('done');
  });
});
