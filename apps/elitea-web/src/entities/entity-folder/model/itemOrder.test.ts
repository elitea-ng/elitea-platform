import { describe, expect, it } from 'vitest';

import { neighboursAt, placeItem } from './itemOrder';

describe('neighboursAt', () => {
  const ids = [1, 2, 3, 4];

  it('names the items either side of the landing index, moving down', () => {
    expect(neighboursAt(ids, 1, 2)).toEqual({ above: 3, below: 4 });
  });

  it('names the items either side of the landing index, moving up', () => {
    expect(neighboursAt(ids, 4, 1)).toEqual({ above: 1, below: 2 });
  });

  it('has a null neighbour at either end', () => {
    expect(neighboursAt(ids, 3, 0)).toEqual({ above: null, below: 1 });
    expect(neighboursAt(ids, 2, 3)).toEqual({ above: 4, below: null });
  });

  it('is undefined for a no-op drop, an index outside the list, or an entity not displayed', () => {
    expect(neighboursAt(ids, 2, 1)).toBeUndefined();
    expect(neighboursAt(ids, 2, -1)).toBeUndefined();
    expect(neighboursAt(ids, 2, 4)).toBeUndefined();
    expect(neighboursAt(ids, 9, 0)).toBeUndefined();
  });
});

describe('placeItem', () => {
  it('puts the entity right after the item above', () => {
    expect(placeItem([1, 2, 3, 4], 1, { above: 3, below: 4 })).toEqual([2, 3, 1, 4]);
  });

  it('puts the entity right before the item below when there is none above', () => {
    expect(placeItem([1, 2, 3, 4], 4, { above: null, below: 1 })).toEqual([4, 1, 2, 3]);
  });

  it('keeps hidden items in place: the neighbours are what was displayed', () => {
    // 5 is in the folder but not on screen (a search hides it).
    expect(placeItem([1, 5, 2, 3], 3, { above: 1, below: 2 })).toEqual([1, 3, 5, 2]);
  });

  it('puts the entity at the top when neither neighbour is in the list', () => {
    expect(placeItem([1, 2], 2, { above: 8, below: 9 })).toEqual([2, 1]);
  });
});
