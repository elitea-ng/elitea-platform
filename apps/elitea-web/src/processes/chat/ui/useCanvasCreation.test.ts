/**
 * The two lookups the create depends on, over the payload shapes the details
 * read actually answers.
 *
 * The behaviour of the whole hook — the read, the create body it sends and the
 * transcript re-render — is stated at the composition root
 * (`./chatCanvasGaps.test.tsx`), because that is the only place a control, a
 * range and a request can be observed as one gesture. What is here is the part
 * a route test would only reach by accident: which ROW a selection resolves to
 * once one answer holds several text items.
 */
import { describe, expect, it } from 'vitest';

import { findGroup, findSelectedItem } from './useCanvasCreation';

const textItem = (id: number, content: string) => ({ id, item_type: 'text_message', item_details: { content } });

describe('findGroup', () => {
  const groups = [
    { id: 900, uuid: 'group-user-1', message_items: [] },
    { id: 901, uuid: 'group-answer-1', message_items: [] },
  ];

  it('matches on the uuid the transcript names a group by', () => {
    expect(findGroup(groups, 'group-answer-1')?.id).toBe(901);
  });

  it('also matches on the row id, for a read that states no uuid', () => {
    expect(findGroup(groups, '901')?.id).toBe(901);
  });

  it('answers nothing rather than the first group when the id is unknown', () => {
    expect(findGroup(groups, 'group-answer-9')).toBeUndefined();
  });
});

describe('findSelectedItem', () => {
  it('picks the item that actually holds the words, not the first text item', () => {
    const group = { id: 901, message_items: [textItem(1, 'before the block'), textItem(3, 'after the block')] };
    expect(findSelectedItem(group, 'after', undefined)?.id).toBe(3);
  });

  it('uses the row id only to break a tie between two items that both hold them', () => {
    const group = { id: 901, message_items: [textItem(1, 'the same words'), textItem(3, 'the same words')] };
    expect(findSelectedItem(group, 'same words', 3)?.id).toBe(3);
    // …and an id that no longer names one of them does not veto the match: an
    // earlier create rewrites the very item a transcript row was rendered from.
    expect(findSelectedItem(group, 'same words', 99)?.id).toBe(1);
  });

  it('ignores a canvas item, which the create route cannot split', () => {
    const group = {
      id: 901,
      message_items: [{ id: 2, item_type: 'canvas_message', item_details: { content: 'carve me' } }, textItem(3, 'not here')],
    };
    expect(findSelectedItem(group, 'carve me', undefined)).toBeUndefined();
  });

  it('answers nothing for an empty selection', () => {
    const group = { id: 901, message_items: [textItem(1, 'anything')] };
    expect(findSelectedItem(group, '   ', undefined)).toBeUndefined();
  });
});
