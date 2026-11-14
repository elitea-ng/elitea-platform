import type { GuardrailNotice } from '@/api/queries/guardrailNotices';
import { readGuardrailNotices } from '@/features/chat/useGuardrailNotices';

const notice: GuardrailNotice = {
  kind: 'redacted', stage: 'input', action: 'mask', guardrail: 'pii', detector: 'personal_data',
  labels: ['email'], requests: 1, first_seen_at: '2026-11-13T10:00:00Z',
};

describe('readGuardrailNotices (issue 2012)', () => {
  it('asks once more when the first read is empty (the verdict was not written yet)', async () => {
    const fetcher = jest.fn().mockResolvedValueOnce([]).mockResolvedValueOnce([notice]);
    expect(await readGuardrailNotices(7, 'run-1', new AbortController().signal, fetcher)).toEqual([notice]);
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it('asks once, not forever, and does not ask again after a found notice', async () => {
    const empty = jest.fn().mockResolvedValue([]);
    expect(await readGuardrailNotices(7, 'run-1', new AbortController().signal, empty)).toEqual([]);
    expect(empty).toHaveBeenCalledTimes(2);
    const found = jest.fn().mockResolvedValue([notice]);
    await readGuardrailNotices(7, 'run-1', new AbortController().signal, found);
    expect(found).toHaveBeenCalledTimes(1);
  });

  it('stops when the turn view goes away', async () => {
    const controller = new AbortController();
    const fetcher = jest.fn().mockImplementation(async () => { controller.abort(); return []; });
    await readGuardrailNotices(7, 'run-1', controller.signal, fetcher);
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
