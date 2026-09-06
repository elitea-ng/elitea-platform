/**
 * Unit tests for the LLM Governance draft ↔ payload mapping (#218).
 *
 * The mapping is where this page can be wrong in a way nothing else catches. A
 * broken render is visible; a payload that drops `limit_usd`, or sends `0` for
 * an empty field, produces a row the server accepts and the gateway enforces
 * as something the operator did not author. `draftToData` and `rowToDraft` are
 * pure, so they are asserted directly rather than through the dialog.
 */
import { describe, expect, it } from 'vitest';

import {
  GOVERNANCE_TYPES,
  isEditableGovernanceRow,
  isGlobalGovernanceType,
  type GovernanceRow,
} from './api/adminGovernanceApi';
import { describeScope } from './GovernanceTable';
import {
  draftToData,
  EMPTY_DRAFT,
  rowToDraft,
  targetWeightSum,
  type GovernanceDraft,
} from './useGatewayGovernancePage';

function draft(patch: Partial<GovernanceDraft>): GovernanceDraft {
  return { ...EMPTY_DRAFT, ...patch };
}

function row(patch: Partial<GovernanceRow>): GovernanceRow {
  return {
    id: 'id-1',
    type: 'budget',
    section: 'governance',
    name: 'row',
    data: {},
    enabled: true,
    ...patch,
  };
}

describe('draftToData', () => {
  it('omits a scope dimension the operator left empty', () => {
    // An empty list and an absent key mean the same thing to the gateway
    // ("all"), but sending `[]` for every dimension would make every row look
    // scoped in the stored document and in any future reader of it.
    const data = draftToData(draft({ type: 'model_config' }));
    expect(data.scope).toBeUndefined();
  });

  it('parses the scope lists from comma-separated text', () => {
    const data = draftToData(
      draft({ type: 'model_config', scopeProjectIds: '7, 8', scopeProviders: 'openai , anthropic' }),
    );
    expect(data.scope).toEqual({ project_ids: [7, 8], providers: ['openai', 'anthropic'] });
  });

  it('omits budget.limit_usd when the field is empty rather than sending 0', () => {
    // This is the assertion that matters most on this page. A `limit_usd` of 0
    // is a ceiling of nothing, which blocks every request for every project the
    // row selects; an ABSENT limit is unlimited.
    const data = draftToData(draft({ type: 'budget', budgetLimitUsd: '' }));
    expect(data.budget).not.toHaveProperty('limit_usd');
  });

  it('omits budget.limit_usd when the row is marked unlimited', () => {
    const data = draftToData(draft({ type: 'budget', budgetIsUnlimited: true, budgetLimitUsd: '250' }));
    expect(data.budget).toMatchObject({ is_unlimited: true });
    expect(data.budget).not.toHaveProperty('limit_usd');
  });

  it('sends the budget limit in USD, unscaled', () => {
    // The gateway scales to nano-USD at the counter boundary and nowhere
    // earlier (design §5.1). Scaling here would be the 1000x class of bug.
    const data = draftToData(draft({ type: 'budget', budgetLimitUsd: '250.50' }));
    expect(data.budget).toMatchObject({ limit_usd: 250.5 });
  });

  it('omits a rate-limit bucket the operator left empty', () => {
    const data = draftToData(draft({ type: 'rate_limit', requestsPerMin: '60', tokensPerMin: '' }));
    expect(data.rate_limit).toEqual({ requests_per_min: 60 });
  });

  it('writes only the group the chosen type uses', () => {
    // A draft carries every type's fields; the payload must not.
    const data = draftToData(
      draft({ type: 'credential_policy', ratePolicy: 'excluded', budgetLimitUsd: '99', cel: 'true' }),
    );
    expect(data).toEqual({ credential: { rate_policy: 'excluded' } });
  });

  it('builds a routing rule with numeric weights', () => {
    const data = draftToData(
      draft({
        type: 'routing_rule',
        cel: 'provider == "openai"',
        priority: '10',
        targets: [
          { provider: 'anthropic', model: 'claude', weight: '0.3' },
          { provider: 'openai', model: 'gpt-4o', weight: '0.7' },
        ],
      }),
    );
    expect(data).toMatchObject({
      cel: 'provider == "openai"',
      priority: 10,
      targets: [
        { provider: 'anthropic', model: 'claude', weight: 0.3 },
        { provider: 'openai', model: 'gpt-4o', weight: 0.7 },
      ],
    });
  });

  it('drops a wholly blank routing target row', () => {
    const data = draftToData(
      draft({
        type: 'routing_rule',
        cel: 'true',
        targets: [
          { provider: 'openai', model: 'gpt-4o', weight: '1' },
          { provider: '', model: '', weight: '0' },
        ],
      }),
    );
    expect(data.targets).toHaveLength(1);
  });

  it('sends an empty MCP allowlist as an empty array', () => {
    // Empty means "every server permitted" and is a real, saveable state — the
    // way an operator turns the allowlist off.
    const data = draftToData(draft({ type: 'mcp_allowlist', mcpAllowlist: '' }));
    expect(data.mcp).toEqual({ allowlist: [] });
  });
});

describe('rowToDraft', () => {
  it('round-trips a budget row through the draft and back', () => {
    const original = row({
      type: 'budget',
      data: {
        scope: { project_ids: [7] },
        budget: { is_unlimited: false, limit_usd: 250, period: 'weekly', soft_alert_pct: 90 },
      },
    });
    const rebuilt = draftToData(rowToDraft(original));
    expect(rebuilt).toEqual(original.data);
  });

  it('round-trips a routing rule', () => {
    const original = row({
      type: 'routing_rule',
      data: {
        cel: 'budget_used > 0.8',
        priority: 5,
        targets: [{ provider: 'anthropic', model: 'claude', weight: 1 }],
      },
    });
    expect(draftToData(rowToDraft(original))).toEqual(original.data);
  });

  it('falls back to one empty target for a rule that stored none', () => {
    // The editor must always render at least one target row, or an operator
    // has no way to add the first one.
    expect(rowToDraft(row({ type: 'routing_rule', data: { cel: 'true' } })).targets).toHaveLength(1);
  });
});

describe('targetWeightSum', () => {
  it('ignores an unparsable weight rather than producing NaN', () => {
    // A half-typed weight must not blank the indicator: NaN renders as "NaN"
    // and tells the operator nothing about the weights they have entered.
    expect(
      targetWeightSum([
        { provider: 'a', model: 'm', weight: '0.5' },
        { provider: 'b', model: 'm', weight: '' },
      ]),
    ).toBe(0.5);
  });
});

describe('describeScope', () => {
  it('says "all projects" rather than leaving the cell empty', () => {
    // An empty cell reads as "not configured" when it means "everything",
    // which is the opposite of what the row does.
    expect(describeScope(row({ type: 'rate_limit', data: {} }))).toBe('all projects');
  });

  it('names the scoped projects', () => {
    expect(describeScope(row({ type: 'rate_limit', data: { scope: { project_ids: [7, 8] } } }))).toContain(
      '7, 8',
    );
  });

  it('describes a model_config row as a permission, not as a selector', () => {
    // For this type the provider and model lists are the ALLOWLIST. Rendering
    // them as "applies to" would state the opposite of the row's effect.
    const described = describeScope(
      row({
        type: 'model_config',
        data: { scope: { project_ids: [7], providers: ['openai'] } },
      }),
    );
    expect(described).toContain('→');
    expect(described).toContain('openai');
    expect(described).toContain('every model');
  });
});

describe('isEditableGovernanceRow', () => {
  it('withholds the editor from the budget-alert row', () => {
    // The budget-alerts surface owns that row and validates its two keys. This
    // page has no field for either, and `draftToData` writes only the groups
    // the chosen type names — so saving it here would drop `enabled` and
    // `threshold_pct` and leave a row that still looked configured.
    expect(isEditableGovernanceRow(row({ type: 'budget_alert', name: 'global' }))).toBe(false);
  });

  it('permits every type this page can actually author', () => {
    for (const type of GOVERNANCE_TYPES) {
      expect(isEditableGovernanceRow(row({ type }))).toBe(true);
    }
  });

  it('proves the data loss it prevents', () => {
    // The failure mode, stated as an assertion rather than as a comment: a
    // budget_alert row put through this page's own mapping comes back without
    // the keys that make it a budget alert.
    const stored = row({
      type: 'budget_alert',
      name: 'global',
      data: { enabled: true, threshold_pct: 80 },
    });
    const roundTripped = draftToData(rowToDraft(stored));
    expect(roundTripped).not.toHaveProperty('enabled');
    expect(roundTripped).not.toHaveProperty('threshold_pct');
  });
});

describe('egress_allowlist rows', () => {
  it('writes the payload the gateway compiles', () => {
    const data = draftToData(
      draft({
        type: 'egress_allowlist',
        egressAllowlist: 'vllm.ml.svc.cluster.local:8000\n192.168.29.0/24, *.openai.azure.com',
      }),
    );
    expect(data.egress).toEqual({
      allowlist: ['vllm.ml.svc.cluster.local:8000', '192.168.29.0/24', '*.openai.azure.com'],
    });
  });

  it('never writes a scope, whatever the draft carries', () => {
    // The row is GLOBAL: half of what it governs is decided by the gateway with
    // no project in hand, so the server refuses a scoped one. A scope written
    // here would produce a 400 naming a field the dialog no longer shows.
    const data = draftToData(
      draft({
        type: 'egress_allowlist',
        egressAllowlist: 'a.example.com',
        scopeProjectIds: '7, 9',
        scopeProviders: 'openai',
        scopeModels: 'gpt-4o',
      }),
    );
    expect(data.scope).toBeUndefined();
    expect(isGlobalGovernanceType('egress_allowlist')).toBe(true);
    expect(isGlobalGovernanceType('budget')).toBe(false);
  });

  it('reads a stored row back into the field', () => {
    const back = rowToDraft(
      row({
        type: 'egress_allowlist',
        name: 'on-prem',
        data: { egress: { allowlist: ['192.168.29.60:8000', '192.168.29.0/24'] } },
      }),
    );
    expect(back.egressAllowlist).toBe('192.168.29.60:8000, 192.168.29.0/24');
  });

  it('describes the destinations in the scope column', () => {
    // "all projects" is true for every egress row and tells the operator
    // nothing. The column shows what the row actually permits.
    expect(
      describeScope(
        row({ type: 'egress_allowlist', data: { egress: { allowlist: ['a.example.com'] } } }),
      ),
    ).toBe('a.example.com');
    expect(describeScope(row({ type: 'egress_allowlist', data: {} }))).toBe('no destinations');
  });
});
