import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { createSubmitGate, createSubmitIds } from './submitGate';

describe('createSubmitGate', () => {
  it('runs one of several submits fired in the same tick (#3077)', async () => {
    const gate = createSubmitGate();
    let runs = 0;
    let release: (value: boolean) => void = () => undefined;
    const submit = (): Promise<boolean> => {
      runs += 1;
      return new Promise<boolean>((resolve) => {
        release = resolve;
      });
    };
    const first = gate.run(submit);
    const second = gate.run(submit);
    const third = gate.run(submit);
    assert.equal(gate.pending, true);
    assert.equal(await second, undefined);
    assert.equal(await third, undefined);
    release(true);
    assert.equal(await first, true);
    assert.equal(runs, 1);
    assert.equal(gate.pending, false);
  });

  it('opens again after a submit settles, also when it throws', async () => {
    const gate = createSubmitGate();
    await assert.rejects(gate.run(() => Promise.reject(new Error('refused'))));
    assert.equal(gate.pending, false);
    assert.equal(await gate.run(() => Promise.resolve(7)), 7);
  });
});

describe('createSubmitIds', () => {
  it('keeps the id until the message is sent, then makes a new one', () => {
    let n = 0;
    const ids = createSubmitIds(() => `id-${String((n += 1))}`);
    assert.equal(ids.current(), 'id-1');
    assert.equal(ids.current(), 'id-1');
    ids.sent();
    assert.equal(ids.current(), 'id-2');
  });

  it('makes lowercase UUIDs by default', () => {
    assert.match(createSubmitIds().current(), /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
  });
});
