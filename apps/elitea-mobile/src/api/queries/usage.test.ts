import {
  SETTINGS_CAPTURE,
  usageBadScope,
  usageProjectMember,
  usageProjectOwner,
  usageUserMember,
  usageUserOwner,
} from '@/api/__fixtures__/settings13';
import { fetchUsage, formatUsd, totalsLine, usageLine, usageOf } from '@/api/queries/usage';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = SETTINGS_CAPTURE.teamProjectId;
const ROUTE = `/elitea_core/usage/prompt_lib/${P}/usage`;

// A capped, used project as the UsageReport schema describes it (the zefir-wave
// stack has no billed calls, so this one is not captured): the captured owner
// payload with a limit, spend and ledger totals filled in.
const cappedOwner = {
  ...usageProjectOwner,
  limit_source: 'explicit',
  enabled: true,
  monthly_limit: 10,
  effective_limit: 10,
  spend: 8.5,
  remaining: 1.5,
  percent_used: 85,
  warning_active: true,
  spend_available: true,
  usage_events_available: true,
  prompt_tokens: 9000,
  completion_tokens: 3345,
  total_tokens: 12345,
  api_requests: 31,
};
const { monthly_limit: _m, effective_limit: _e, spend: _s, remaining: _r, currency: _c, ...redacted } = cappedOwner;
const cappedMember = { ...redacted, can_see_amounts: false };

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
});

describe('usage (client contract 1.3, captured shapes)', () => {
  it('reads an unlimited project with no ledger rows as "No limit" and no totals', () => {
    const owner = usageOf(usageProjectOwner)!;
    expect(owner).toMatchObject({ scope: 'project', canSeeAmounts: true, unlimited: true, spendAvailable: false, spend: null, totals: null, warning: false });
    expect(owner).toMatchObject({ periodStart: '2026-10-01', periodEnd: '2026-10-31', resetsAt: '2026-11-01T00:00:00Z' });
    expect(usageLine(owner)).toBe('No limit');
    expect(totalsLine(owner)).toBeNull();
    expect(usageOf(usageUserOwner)).toMatchObject({ scope: 'user', unlimited: true });
  });

  it('a member without cost access gets no amounts (the server removed them)', () => {
    for (const body of [usageProjectMember, usageUserMember]) {
      const u = usageOf(body)!;
      expect(u).toMatchObject({ canSeeAmounts: false, spend: null, limit: null, remaining: null });
      expect(usageLine(u)).toBe('No limit');
    }
  });

  it('shows spend of limit with the percentage, and the totals once the ledger has rows', () => {
    const u = usageOf(cappedOwner)!;
    expect(usageLine(u)).toBe('$8.50 of $10.00 (85%)');
    expect(totalsLine(u)).toBe('12,345 tokens · 31 requests');
    expect(u.warning).toBe(true);
    expect(usageLine(usageOf(cappedMember)!)).toBe('85% of the budget');
  });

  // F3: spend_available false means the server has no accumulator row for the
  // period, which is "no usage data", not "$0.00 spent".
  it('a limit with no accumulator row reads as "no usage data", not $0.00 of the limit', () => {
    const noRow = { ...cappedOwner, spend: 0, remaining: 10, percent_used: 0, warning_active: false, spend_available: false, usage_events_available: false };
    const u = usageOf(noRow)!;
    expect(u).toMatchObject({ spendAvailable: false, spend: null, remaining: null, percentUsed: null, limit: 10 });
    expect(usageLine(u)).toBe('Limit $10.00, no usage data yet');
    const { monthly_limit: _ml, effective_limit: _el, spend: _sp, remaining: _rm, currency: _cu, ...hidden } = noRow;
    expect(usageLine(usageOf({ ...hidden, can_see_amounts: false })!)).toBe('Budget set, no usage data yet');
    // Unlimited with no row stays "No limit" (the captured shapes).
    expect(usageLine(usageOf(usageProjectOwner)!)).toBe('No limit');
  });

  it('formats tiny amounts so they do not read as zero', () => {
    expect(formatUsd(0)).toBe('$0.00');
    expect(formatUsd(0.0042)).toBe('$0.0042');
    expect(formatUsd(12.3)).toBe('$12.30');
  });

  it('asks for the scope, and answers null for a refusal or an older server', async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, { status: 200, body: usageUserOwner }, { status: 400, body: usageBadScope }, { status: 404, body: {} });
    await expect(fetchUsage(P, 'user', server.client)).resolves.toMatchObject({ scope: 'user' });
    expect(server.requests[0].url.searchParams.get('scope')).toBe('user');
    await expect(fetchUsage(P, 'project', server.client)).resolves.toBeNull();
    await expect(fetchUsage(P, 'project', server.client)).resolves.toBeNull();
  });

  it('a body that is not a usage report is null, not zeros', () => {
    expect(usageOf({ ok: true })).toBeNull();
  });
});
