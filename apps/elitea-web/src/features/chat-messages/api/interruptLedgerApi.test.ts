import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { decideLedgerInterrupt } from './interruptLedgerApi';

const BASE = '/api/v2';
const RESPONSE = '10000000-0000-4000-8000-000000000043';
const KEY = '0b7672bbbf4eb882481748fb8a0ce16c20f873b838e311a64e6442376c7a5a25';
const BODY = { request_id: KEY, expected_revision: 1, action: 'authorize', value: '', credential_ref: 'tsr_4f9a1c2b7d3e8f60' } as const;
const DECISION = `${BASE}/elitea_core/task/prompt_lib/7/${RESPONSE}/interrupts/${KEY}/decision`;

function conflictWith(error: string): void {
  server.use(http.post(DECISION, () => HttpResponse.json({ error, message: 'm', retryable: false }, { status: 409 })));
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('decideLedgerInterrupt (FU-250)', () => {
  it('keeps a refused sign-in open instead of treating it as answered elsewhere', async () => {
    conflictWith('agent_interrupt_credential_refused');
    await expect(decideLedgerInterrupt('7', RESPONSE, KEY, BODY as never)).resolves.toEqual({ kind: 'refused', reason: 'credential_refused' });
  });

  it('still reads any other 409 as answered elsewhere', async () => {
    conflictWith('agent_interrupt_already_resolved');
    await expect(decideLedgerInterrupt('7', RESPONSE, KEY, BODY as never)).resolves.toEqual({ kind: 'consumed' });
  });
});
