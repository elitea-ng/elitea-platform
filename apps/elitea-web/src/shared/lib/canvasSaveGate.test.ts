import { describe, expect, it } from 'vitest';

import { settleCanvasSaves, trackCanvasSave } from './canvasSaveGate';

describe('canvasSaveGate', () => {
  it('resolves at once with nothing in flight', async () => {
    await expect(settleCanvasSaves()).resolves.toBeUndefined();
  });

  it('waits for every tracked save, and a failed one does not reject the send', async () => {
    const order: string[] = [];
    let resolveOk: () => void = () => undefined;
    let rejectBad: (error: Error) => void = () => undefined;
    const ok = new Promise<void>((resolve) => { resolveOk = resolve; }).then(() => { order.push('ok'); });
    const bad = new Promise<void>((_resolve, reject) => { rejectBad = reject; });
    bad.catch(() => { order.push('bad'); });
    trackCanvasSave(ok);
    trackCanvasSave(bad);

    const settled = settleCanvasSaves().then(() => { order.push('send'); });
    resolveOk();
    rejectBad(new Error('413'));
    await settled;
    expect(order.at(-1)).toBe('send');
    expect(order).toContain('ok');
    // Settled saves leave the set: the next send does not wait on them.
    await expect(settleCanvasSaves()).resolves.toBeUndefined();
  });
});
