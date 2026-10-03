/**
 * Admin › LLM Governance — the authoring surface for `gateway.governance_config`
 * (issue #218).
 *
 * ## Why this page exists
 *
 * The admin Configuration page has an LLM Governance section, and that section
 * is unavailable. Its `unavailable_reason` has always pointed the operator at
 * `/admin/gateway/governance` — which was an elitea-main REST route with no
 * screen behind it in this SPA. So the pointer led nowhere, and the definitions
 * it led to were not enforced either: the gateway had never read the table.
 *
 * Both halves are closed now. The gateway reads every enabled row and enforces
 * it on the `/llm` path (`services/elitea-llm-gateway/internal/policy`), and
 * this is the screen the reason names.
 *
 * The section stays unavailable on the Configuration page, for the narrower and
 * older reason: that page is a flat form over one value document, and a
 * governance corpus is a list of rows each with its own scope. A row editor is
 * the only shape that can express it.
 *
 * ## What an operator needs to know, on the page rather than in a runbook
 *
 * Two things about this surface are surprising, and both are stated in the UI:
 *
 *  1. **A save is not instant.** The gateway polls this table
 *     (`LLM_GOVERNANCE_REFRESH_SEC`, 30 s by default). There is no event-driven
 *     reload, deliberately — a replica that missed an event must still
 *     converge — so a definition takes effect within that window.
 *  2. **A saved row can still be unenforceable.** A rule may load and match
 *     nothing, or be rejected at load for a reason this form could not catch.
 *     The gateway reports both on its own `GET /governance/status`, and the
 *     page says where to look rather than implying that "saved" means "in
 *     force". Nothing here can query that endpoint: the admin SPA does not talk
 *     to the gateway (design-governance-config-authoring §5), and inventing a
 *     proxy for it is a separate decision.
 *
 * ## Authorisation
 *
 * Every route this page calls is gated server-side on
 * `configuration.governance` (`internal/api/router.go`, `RequireCentralPermissions`
 * in administration mode). `window.admin_ui_config.permissions` hides the nav
 * item and never gates anything — see `./adminUiConfig`.
 */
import AddIcon from '@mui/icons-material/Add';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { GovernanceDialog } from './GovernanceDialog';
import { GovernanceTable } from './GovernanceTable';
import { useGatewayGovernancePage } from './useGatewayGovernancePage';

export function AdminGatewayGovernance() {
  const state = useGatewayGovernancePage();

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Box
        sx={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          gap: '1rem',
          flexWrap: 'wrap',
        }}
      >
        <Typography variant="headingLarge" component="h1">
          {t('pages.admin.governance.title', 'LLM Governance')}
        </Typography>
        <Button
          variant="elitea" color="primary"
          size="small"
          startIcon={<AddIcon />}
          onClick={state.onCreate}
          data-testid="governance-create"
        >
          {t('pages.admin.governance.action.create', 'New entry')}
        </Button>
      </Box>

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.governance.subtitle',
          'Budgets, rate limits, model and MCP allowlists, credential rate policy and CEL routing rules. The LLM gateway enforces these on every request.',
        )}
      </Typography>

      <Alert severity="info" data-testid="governance-propagation-notice">
        {t(
          'pages.admin.governance.propagation',
          'A saved definition takes effect within the gateway’s refresh interval (30 seconds by default), not immediately. The gateway’s /governance/status endpoint reports what it has loaded, and names any entry it rejected or that can match nothing.',
        )}
      </Alert>

      {state.loadError === undefined ? null : (
        <Alert severity="warning" data-testid="governance-load-error">
          {state.loadError}
        </Alert>
      )}
      {state.deleteError === undefined ? null : (
        <Alert severity="error" data-testid="governance-delete-error">
          {state.deleteError}
        </Alert>
      )}

      {state.isLoading ? <LinearProgress /> : null}

      <GovernanceTable
        rows={state.rows}
        search={state.search}
        onSearchChange={state.onSearchChange}
        onEdit={state.onEdit}
        onDelete={state.onDelete}
      />

      {state.draft === undefined ? null : (
        <GovernanceDialog
          draft={state.draft}
          isNew={state.isNew}
          isSaving={state.isSaving}
          saveError={state.saveError}
          onChange={state.onDraftChange}
          onCancel={state.onCancel}
          onSave={state.onSave}
        />
      )}
    </DrawerPage>
  );
}
