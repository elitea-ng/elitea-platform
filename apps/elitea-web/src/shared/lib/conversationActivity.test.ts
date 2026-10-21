import { afterEach, describe, expect, it } from 'vitest';

import {
  activityGlyph,
  activityTitleBold,
  applyActivityFrame,
  confirmReadLocally,
  countNeedsYou,
  markReadLocally,
  mergeRowActivity,
  parseActivityFrame,
  parseActivityWire,
  resetConversationActivityOverlay,
  resolveRowActivity,
  type ActivityRowFields,
} from './conversationActivity';

const NOW = Date.parse('2026-10-20T12:00:00Z');
const at = (offsetSeconds: number): string => new Date(NOW + offsetSeconds * 1000).toISOString();

const row = (overrides: Partial<ActivityRowFields> = {}): ActivityRowFields => ({
  id: '5',
  activity: { state: 'idle', since: at(-60) },
  lastActivityAt: at(-60),
  unread: false,
  ...overrides,
});

const frame = (fields: Record<string, unknown>): string => JSON.stringify({ conversation_id: 5, ...fields });

afterEach(() => resetConversationActivityOverlay());

describe('parsing', () => {
  it('reads the wire activity, with an unknown state as idle', () => {
    expect(parseActivityWire({ state: 'needs_you', since: at(0) })).toEqual({ state: 'needs_you', since: at(0) });
    expect(parseActivityWire({ state: 'thinking' })).toEqual({ state: 'idle' });
    expect(parseActivityWire(undefined)).toBeUndefined();
  });

  it('parses only a frame with a conversation id', () => {
    expect(parseActivityFrame(frame({ conversation_uuid: 'ABC', state: 'running', since: at(0), last_activity_at: at(0) }))).toEqual({
      conversationId: '5',
      conversationUuid: 'abc',
      state: 'running',
      since: at(0),
      lastActivityAt: at(0),
    });
    expect(parseActivityFrame(frame({ unread_hint: false, read_at: at(1) }))).toEqual({ conversationId: '5', unreadHint: false, readAt: at(1) });
    for (const bad of ['nope', 'null', '{}', JSON.stringify({ conversation_id: '5' }), JSON.stringify({ conversation_id: -1 })]) {
      expect(parseActivityFrame(bad)).toBeNull();
    }
  });
});

describe('the overlay', () => {
  it('applies a newer frame, and never an older or a duplicate one', () => {
    const running = parseActivityFrame(frame({ state: 'running', since: at(10), last_activity_at: at(10) }));
    applyActivityFrame(running!);
    expect(resolveRowActivity(row())).toMatchObject({ state: 'running', unread: false });

    const finished = parseActivityFrame(frame({ state: 'idle', since: at(20), last_activity_at: at(20), unread_hint: true }));
    applyActivityFrame(finished!);
    applyActivityFrame(finished!);
    // An older running frame that arrives late changes nothing.
    applyActivityFrame(running!);
    expect(resolveRowActivity(row())).toMatchObject({ state: 'idle', lastActivityAt: at(20), unread: true });
  });

  it('keeps a listing that is newer than the frame', () => {
    applyActivityFrame(parseActivityFrame(frame({ state: 'running', last_activity_at: at(10) }))!);
    const newer = row({ activity: { state: 'needs_you', since: at(30) }, lastActivityAt: at(30), unread: true });
    expect(resolveRowActivity(newer)).toMatchObject({ state: 'needs_you', unread: true });
  });

  it('does not make a row unread from a hint for the activity the listing already has', () => {
    applyActivityFrame(parseActivityFrame(frame({ state: 'idle', last_activity_at: at(-60), unread_hint: true }))!);
    expect(resolveRowActivity(row()).unread).toBe(false);
  });

  it('reads a row locally, then by the server marker, and a later answer is unread again', () => {
    const unread = row({ unread: true });
    markReadLocally('5', NOW);
    expect(resolveRowActivity(unread).unread).toBe(false);
    confirmReadLocally('5', at(-30));
    expect(resolveRowActivity(unread).unread).toBe(false);
    applyActivityFrame(parseActivityFrame(frame({ state: 'idle', last_activity_at: at(90), unread_hint: true }))!);
    expect(resolveRowActivity(unread).unread).toBe(true);
  });

  it('takes the unread hint from each frame: a turn that starts again clears the previous answer\'s', () => {
    applyActivityFrame(parseActivityFrame(frame({ state: 'idle', last_activity_at: at(20), unread_hint: true }))!);
    expect(resolveRowActivity(row()).unread).toBe(true);
    applyActivityFrame(parseActivityFrame(frame({ state: 'running', last_activity_at: at(30) }))!);
    expect(resolveRowActivity(row())).toMatchObject({ state: 'running', unread: false });
    applyActivityFrame(parseActivityFrame(frame({ state: 'needs_you', last_activity_at: at(40), unread_hint: true }))!);
    expect(resolveRowActivity(row())).toMatchObject({ state: 'needs_you', unread: true });
  });

  it('never moves the read marker back on the server\'s answer', () => {
    const unread = row({ unread: true, lastActivityAt: at(-10) });
    markReadLocally('5', NOW);
    confirmReadLocally('5', at(-30));
    expect(resolveRowActivity(unread).unread).toBe(false);
  });

  it('reads optimistically up to now only, and a failed read puts the previous marker back', () => {
    // An answer after the client's clock stays unread: no allowance hides it.
    expect(markReadLocally('5', NOW)).toBeTypeOf('function');
    expect(resolveRowActivity(row({ unread: true, lastActivityAt: at(30) })).unread).toBe(true);

    resetConversationActivityOverlay();
    const unread = row({ unread: true });
    const undo = markReadLocally('5', NOW);
    expect(resolveRowActivity(unread).unread).toBe(false);
    undo();
    expect(resolveRowActivity(unread).unread).toBe(true);

    // A newer marker that arrived meanwhile (another device) is kept.
    const undoLater = markReadLocally('5', NOW);
    applyActivityFrame(parseActivityFrame(frame({ unread_hint: false, read_at: at(60) }))!);
    undoLater();
    expect(resolveRowActivity(unread).unread).toBe(false);
  });

  it('applies a read frame of another device', () => {
    applyActivityFrame(parseActivityFrame(frame({ unread_hint: false, read_at: at(0) }))!);
    expect(resolveRowActivity(row({ unread: true })).unread).toBe(false);
    // An answer after that read stays unread.
    expect(resolveRowActivity(row({ unread: true, lastActivityAt: at(5) })).unread).toBe(true);
  });

  it('finds a row by its UUID', () => {
    applyActivityFrame(parseActivityFrame(frame({ conversation_uuid: 'u-1', state: 'running', last_activity_at: at(10) }))!);
    expect(resolveRowActivity({ id: 'local-thread', uuid: 'U-1' }).state).toBe('running');
  });
});

describe('what a row shows', () => {
  it('picks the glyph and the weight', () => {
    const merged = (fields: Partial<ActivityRowFields>) => mergeRowActivity(row(fields), undefined);
    expect(activityGlyph(merged({ activity: { state: 'running' } }), NOW)).toBe('running');
    expect(activityGlyph(merged({ activity: { state: 'needs_you' } }), NOW)).toBe('needs_you');
    expect(activityGlyph(merged({ unread: true }), NOW)).toBe('unread');
    expect(activityGlyph(merged({}), NOW)).toBe('done');
    // Old: no glyph; old and unread: the bold title alone.
    const old = merged({ lastActivityAt: at(-3 * 24 * 3600), unread: true });
    expect(activityGlyph(old, NOW)).toBeNull();
    expect(activityTitleBold(old)).toBe(true);
    expect(activityTitleBold(merged({ activity: { state: 'running' }, unread: true }))).toBe(false);
  });

  it('counts needs you over the whole group, live for the rows it holds', () => {
    const rows = [row({ id: '1', activity: { state: 'needs_you' } }), row({ id: '2' })];
    expect(countNeedsYou(rows, 3)).toBe(3);
    expect(countNeedsYou(rows)).toBe(1);
    applyActivityFrame(parseActivityFrame(JSON.stringify({ conversation_id: 2, state: 'needs_you', last_activity_at: at(10) }))!);
    applyActivityFrame(parseActivityFrame(JSON.stringify({ conversation_id: 1, state: 'idle', last_activity_at: at(10) }))!);
    expect(countNeedsYou(rows, 3)).toBe(3);
    applyActivityFrame(parseActivityFrame(JSON.stringify({ conversation_id: 2, state: 'idle', last_activity_at: at(20) }))!);
    expect(countNeedsYou(rows, 3)).toBe(2);
  });
});
