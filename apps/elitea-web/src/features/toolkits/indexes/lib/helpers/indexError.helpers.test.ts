import { describe, expect, it } from 'vitest';

import { describeIndexError } from './indexError.helpers';

describe('describeIndexError', () => {
  it('turns the bare gateway refusal of an old run into a sentence and keeps the code', () => {
    const raw = "Index 'test 3' failed: the embedding gateway refused the run (model_gateway.forbidden); nothing further was embedded";
    const text = describeIndexError(raw);
    expect(text).toContain('(model_gateway.forbidden): this project is not allowed to use that model');
    expect(text).toContain('nothing further was embedded');
  });

  it('reads an unknown code exactly as the worker does for one', () => {
    const unknown = 'the embedding gateway refused the run (model_gateway.brand_new); nothing further was embedded';
    expect(describeIndexError(unknown)).toBe(
      'the embedding gateway refused the run (model_gateway.brand_new): the gateway did not accept the request; nothing further was embedded',
    );
  });

  it('leaves a message that already explains itself as written', () => {
    const current = "Index 'x' failed: the embedding gateway refused the run (model_gateway.forbidden): this project is not allowed to use that model; nothing further was embedded";
    expect(describeIndexError(current)).toBe(current);
  });

  it('is undefined when the index carries no failure text, and bounds a very long one', () => {
    expect(describeIndexError(undefined)).toBeUndefined();
    expect(describeIndexError('   ')).toBeUndefined();
    expect(describeIndexError(42)).toBeUndefined();
    expect(describeIndexError('x'.repeat(5000))).toHaveLength(601);
  });
});
