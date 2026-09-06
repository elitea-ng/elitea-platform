/**
 * The per-type field groups of the LLM Governance dialog (#218).
 *
 * Split out of `GovernanceDialog.tsx` so neither file grows past what a reader
 * can hold: the dialog is the frame, the save path and the type picker; this is
 * what each `type` actually asks for.
 *
 * ## The client guides, the server decides
 *
 * The Σ = 1.0 indicator and the CEL check here are HINTS. Every write is
 * re-validated on the server, which compiles the CEL, re-verifies the weight
 * sum, and refuses a scope it cannot evaluate. A rule that fails there is
 * rejected whatever this dialog showed, and the server's reason is what the
 * page displays (design-governance-config-authoring §3.1).
 */
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import { BUDGET_PERIODS, NATS_FAIL_MODES, RATE_POLICIES } from './api/adminGovernanceApi';
import { RoutingFields } from './GovernanceRoutingFields';
import type { GovernanceDraft } from './useGatewayGovernancePage';

export interface GovernanceDialogProps {
  readonly draft: GovernanceDraft;
  readonly isNew: boolean;
  readonly isSaving: boolean;
  readonly saveError: string | undefined;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
  readonly onCancel: () => void;
  readonly onSave: () => void;
}

export function typeLabel(type: string): string {
  switch (type) {
    case 'budget':
      return t('pages.admin.governance.type.budget', 'Budget');
    case 'rate_limit':
      return t('pages.admin.governance.type.rateLimit', 'Rate limit');
    case 'model_config':
      return t('pages.admin.governance.type.modelConfig', 'Model & provider allowlist');
    case 'mcp_allowlist':
      return t('pages.admin.governance.type.mcpAllowlist', 'MCP server allowlist');
    case 'credential_policy':
      return t('pages.admin.governance.type.credentialPolicy', 'Credential rate policy');
    case 'routing_rule':
      return t('pages.admin.governance.type.routingRule', 'CEL routing rule');
    case 'egress_allowlist':
      return t('pages.admin.governance.type.egressAllowlist', 'Egress allowlist');
    default:
      return type;
  }
}

/** The one-line explanation of what the chosen type does, shown under the picker. */
export function typeHelp(type: string): string {
  switch (type) {
    case 'budget':
      return t(
        'pages.admin.governance.help.budget',
        'A spend ceiling in USD. It applies to a project that has no budget of its own; a per-project budget always wins.',
      );
    case 'rate_limit':
      return t(
        'pages.admin.governance.help.rateLimit',
        'Requests and tokens per minute, counted per project. The token ceiling is applied to the request after the one that crossed it, because a request’s token cost is unknown until the provider answers.',
      );
    case 'model_config':
      return t(
        'pages.admin.governance.help.modelConfig',
        'The providers and models the selected projects may use. Leave both empty to exempt those projects from every other allowlist row.',
      );
    case 'mcp_allowlist':
      return t(
        'pages.admin.governance.help.mcpAllowlist',
        'The MCP servers a request may name. An empty list turns the allowlist off and permits every server.',
      );
    case 'credential_policy':
      return t(
        'pages.admin.governance.help.credentialPolicy',
        'How usage is accounted: billed normally, metered at zero cost, or excluded from accounting entirely.',
      );
    case 'routing_rule':
      return t(
        'pages.admin.governance.help.routingRule',
        'A CEL predicate plus weighted targets. When it matches, the request is dispatched to one of the targets instead. A routed target is still judged by the model allowlist.',
      );
    case 'egress_allowlist':
      return t(
        'pages.admin.governance.help.egressAllowlist',
        'The hosts a provider credential may point at. It applies to every project, and it is added to the GATEWAY_EGRESS_ALLOWLIST the chart sets; nothing here can withdraw a host the chart named.',
      );
    default:
      return '';
  }
}

export function ScopeFields({
  draft,
  onChange,
}: {
  readonly draft: GovernanceDraft;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
}) {
  const isAllowlist = draft.type === 'model_config';
  return (
    <>
      <TextField
        label={t('pages.admin.governance.field.projectIds', 'Projects')}
        helperText={t(
          'pages.admin.governance.field.projectIdsHelp',
          'Comma-separated project ids. Empty means every project.',
        )}
        value={draft.scopeProjectIds}
        onChange={(event) => onChange({ scopeProjectIds: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'governance-scope-projects' } }}
      />
      <TextField
        label={
          isAllowlist
            ? t('pages.admin.governance.field.allowedProviders', 'Permitted providers')
            : t('pages.admin.governance.field.providers', 'Providers')
        }
        helperText={
          isAllowlist
            ? t(
                'pages.admin.governance.field.allowedProvidersHelp',
                'Comma-separated provider ids the selected projects may use. Empty means every provider.',
              )
            : t(
                'pages.admin.governance.field.providersHelp',
                'Comma-separated provider ids this entry applies to. Empty means every provider.',
              )
        }
        value={draft.scopeProviders}
        onChange={(event) => onChange({ scopeProviders: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'governance-scope-providers' } }}
      />
      <TextField
        label={
          isAllowlist
            ? t('pages.admin.governance.field.allowedModels', 'Permitted models')
            : t('pages.admin.governance.field.models', 'Models')
        }
        helperText={
          isAllowlist
            ? t(
                'pages.admin.governance.field.allowedModelsHelp',
                'Comma-separated model names the selected projects may use. Empty means every model.',
              )
            : t(
                'pages.admin.governance.field.modelsHelp',
                'Comma-separated model names this entry applies to. Empty means every model.',
              )
        }
        value={draft.scopeModels}
        onChange={(event) => onChange({ scopeModels: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'governance-scope-models' } }}
      />
    </>
  );
}

function BudgetFields({
  draft,
  onChange,
}: {
  readonly draft: GovernanceDraft;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
}) {
  return (
    <>
      <FormControlLabel
        control={
          <Switch
            checked={draft.budgetIsUnlimited}
            onChange={(event) => onChange({ budgetIsUnlimited: event.target.checked })}
            data-testid="governance-budget-unlimited"
          />
        }
        label={t('pages.admin.governance.field.unlimited', 'Unlimited (no ceiling)')}
      />
      {draft.budgetIsUnlimited ? null : (
        <TextField
          label={t('pages.admin.governance.field.limitUsd', 'Limit (USD)')}
          helperText={t(
            'pages.admin.governance.field.limitUsdHelp',
            'Authored in US dollars. Leave empty for no ceiling — this is not the same as 0, which would block every request.',
          )}
          value={draft.budgetLimitUsd}
          onChange={(event) => onChange({ budgetLimitUsd: event.target.value })}
          size="small"
          fullWidth
          slotProps={{ htmlInput: { inputMode: 'decimal', 'data-testid': 'governance-budget-limit' } }}
        />
      )}
      <TextField
        select
        label={t('pages.admin.governance.field.period', 'Period')}
        value={draft.budgetPeriod}
        onChange={(event) => onChange({ budgetPeriod: event.target.value })}
        size="small"
        fullWidth
      >
        {BUDGET_PERIODS.map((period) => (
          <MenuItem key={period} value={period}>
            {period}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        label={t('pages.admin.governance.field.softAlertPct', 'Soft alert threshold (%)')}
        helperText={t(
          'pages.admin.governance.field.softAlertPctHelp',
          'Between 1 and 100. An alert is emitted at this share of the ceiling, before it is reached.',
        )}
        value={draft.budgetSoftAlertPct}
        onChange={(event) => onChange({ budgetSoftAlertPct: event.target.value })}
        size="small"
        fullWidth
        slotProps={{ htmlInput: { inputMode: 'numeric' } }}
      />
      <TextField
        select
        label={t('pages.admin.governance.field.failMode', 'Fail mode')}
        helperText={t(
          'pages.admin.governance.field.failModeHelp',
          'How the gateway behaves when the budget counter is unavailable. Leave empty to inherit the platform default.',
        )}
        value={draft.budgetNatsFailMode}
        onChange={(event) => onChange({ budgetNatsFailMode: event.target.value })}
        size="small"
        fullWidth
      >
        <MenuItem value="">{t('pages.admin.governance.field.inherit', 'Inherit the default')}</MenuItem>
        {NATS_FAIL_MODES.map((mode) => (
          <MenuItem key={mode} value={mode}>
            {mode}
          </MenuItem>
        ))}
      </TextField>
    </>
  );
}

export function TypeFields({
  draft,
  onChange,
}: {
  readonly draft: GovernanceDraft;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
}) {
  switch (draft.type) {
    case 'budget':
      return <BudgetFields draft={draft} onChange={onChange} />;
    case 'rate_limit':
      return (
        <>
          <TextField
            label={t('pages.admin.governance.field.requestsPerMin', 'Requests per minute')}
            helperText={t(
              'pages.admin.governance.field.requestsPerMinHelp',
              'Leave empty for no request limit. Counted per project, even for a rule that names no project.',
            )}
            value={draft.requestsPerMin}
            onChange={(event) => onChange({ requestsPerMin: event.target.value })}
            size="small"
            fullWidth
            slotProps={{ htmlInput: { inputMode: 'numeric', 'data-testid': 'governance-requests-per-min' } }}
          />
          <TextField
            label={t('pages.admin.governance.field.tokensPerMin', 'Tokens per minute')}
            helperText={t(
              'pages.admin.governance.field.tokensPerMinHelp',
              'Leave empty for no token limit. Enforced on the request after the one that crossed it.',
            )}
            value={draft.tokensPerMin}
            onChange={(event) => onChange({ tokensPerMin: event.target.value })}
            size="small"
            fullWidth
            slotProps={{ htmlInput: { inputMode: 'numeric' } }}
          />
        </>
      );
    case 'credential_policy':
      return (
        <TextField
          select
          label={t('pages.admin.governance.field.ratePolicy', 'Rate policy')}
          value={draft.ratePolicy}
          onChange={(event) => onChange({ ratePolicy: event.target.value })}
          size="small"
          fullWidth
        >
          {RATE_POLICIES.map((policy) => (
            <MenuItem key={policy} value={policy}>
              {policy}
            </MenuItem>
          ))}
        </TextField>
      );
    case 'mcp_allowlist':
      return (
        <TextField
          label={t('pages.admin.governance.field.mcpAllowlist', 'Permitted MCP servers')}
          helperText={t(
            'pages.admin.governance.field.mcpAllowlistHelp',
            'Comma-separated server labels or host names. An empty list permits every server.',
          )}
          value={draft.mcpAllowlist}
          onChange={(event) => onChange({ mcpAllowlist: event.target.value })}
          size="small"
          fullWidth
          multiline
          minRows={2}
          slotProps={{ htmlInput: { 'data-testid': 'governance-mcp-allowlist' } }}
        />
      );
    case 'routing_rule':
      return <RoutingFields draft={draft} onChange={onChange} />;
    case 'egress_allowlist':
      return (
        <TextField
          label={t('pages.admin.governance.field.egressAllowlist', 'Permitted destinations')}
          helperText={t(
            'pages.admin.governance.field.egressAllowlistHelp',
            'One entry per line, or comma separated: host, host:port, *.domain, or a CIDR block such as 192.168.29.0/24. The FIRST entry turns the restriction on for every provider credential. A private address is only reachable when an entry names that address or its block \u2014 a host name alone does not unlock it, because the gateway never resolves a name to decide.',
          )}
          value={draft.egressAllowlist}
          onChange={(event) => onChange({ egressAllowlist: event.target.value })}
          size="small"
          fullWidth
          multiline
          minRows={3}
          slotProps={{ htmlInput: { 'data-testid': 'governance-egress-allowlist' } }}
        />
      );
    case 'model_config':
      return null;
  }
}

