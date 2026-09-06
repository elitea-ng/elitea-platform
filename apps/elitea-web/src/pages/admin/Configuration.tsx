/**
 * Admin › Configuration — unit A14, issue #200.
 *
 * Reference (read-only): `apps/admin-ui/frontend/src/pages/ConfigurationPage/`
 * plus its `components/SchemaForm/**` (≈5,400 lines together). Rewritten against
 * this app's stack: Redux Toolkit → TanStack Query (`./api/adminConfigurationApi`),
 * axios → `eliteaFetch`, react-router → TanStack Router (`./router`), MUI 7 → 9.
 * State and handlers live in `./useAdminConfigurationPage`.
 *
 * ## What this page configures, and where that lives
 *
 * The reference page is not a form over a settings table. pylon has no settings
 * table. Every pylon announces its loaded plugins every 15 seconds over the
 * Arbiter bus — including each plugin's parsed config, its raw YAML and an
 * `admin_schema.json` — and the admin plugin keeps the last announcement of each
 * in an in-process dict with a 60-second freshness cut-off. The page reads that
 * dict; saving re-serialises the affected plugin's whole YAML and fires
 * `bootstrap_runtime_update` fire-and-forget, and a handler on the target pylon
 * writes the bytes into THAT pylon's `plugin_config` table and reloads the
 * descriptor (legacy/plugins/admin/api/v2/plugin_config_values.py).
 *
 * That is Pylon plugin loading over Arbiter transport, which AGENTS.md names
 * explicitly as things the target architecture does NOT preserve. So the
 * sections divide, and the division is declared BY THE SERVER — every section
 * carries an `unavailable_reason` or none, and the value endpoints answer 501
 * with that same string. This page renders what it is told; it does not decide.
 *
 *   - **LLM Proxy** — Pylon plugin configuration. Unavailable, with the reason.
 *   - **Guardrails** — live, and the first section that left the list above. Its
 *     fields address `toolkit_security.*` like a plugin's, but the test was
 *     never "the path looks like a plugin's": it is "does anything here read
 *     the values". Four things now do.
 *   - **MCP Servers** and **Authentication** — available through DEDICATED
 *     surfaces, declared by the server as a `managed_surface`. Both KEEP their
 *     `unavailable_reason`, and both keep it for the same reason: their data
 *     includes a credential, and the plugin-config value endpoints store a
 *     section's fields as plaintext rows readable by every holder of
 *     `runtime.plugins`. So the value endpoints still refuse, and the editor is
 *     somewhere those rows are not involved.
 *   - **Banner** — LIVE. It was unavailable for the narrower reason that
 *     nothing in this platform read it YET, which was true twice over: the
 *     legacy SPA took the banner from a BUILD-TIME `VITE_MAINTENANCE_BANNER`
 *     variable, so the rows this form wrote were read by nothing in either
 *     deployment. `platform_settings` now publishes them and
 *     `widgets/app-shell` renders them, which also turns a redeploy into a
 *     switch — the difference that matters for a control whose purpose is to
 *     say something is happening right now.
 *   - **LLM Governance** — authored on its own page (`./GatewayGovernance.tsx`,
 *     `/admin/app/governance`), not here. It is the ONE section whose reason
 *     changed rather than whose status did: it was withheld because nothing
 *     read `gateway.governance_config`, and #218 made the LLM gateway read and
 *     enforce every enabled row. It stays unavailable HERE because a governance
 *     corpus is a list of scoped rows and this page is a flat form over one
 *     value document — the row editor is the only shape that can express it.
 *   - **Maintenance** — LIVE, and ENFORCED. It was withheld because the switch
 *     "would toggle a setting nothing enforces": pylon's maintenance mode was a
 *     request hook on a plugin runtime, and this service installs no hooks.
 *     elitea-main's Maintenance middleware is the port — 503 for every caller
 *     without administration access, the admin surface and the auth routes
 *     exempt so the window stays reversible from inside itself.
 *   - **Advanced** — GONE, not unavailable. Its subject is Pylon plugin
 *     loading, which the target architecture removes on purpose, so there was
 *     nothing to wait for. A withheld section is a promise that the capability
 *     is coming; this was the one section that could never keep it, and a
 *     permanent row saying "not available here" spends an operator's attention
 *     on a control that will never exist.
 *   - **Observability, Runtime, Admin Panel** — GONE too, and for the same
 *     reason as Advanced: every field in the three addressed a per-process
 *     environment variable, a deployment-mounted YAML file, or a table this
 *     service already exposes through its own admin surface (Schedules &
 *     Tasks) — nowhere this page's save could ever reach. The server's
 *     `config_schemas.go` records the per-field detail. The one field worth
 *     keeping, `analytics_enabled`, moved to the Features page instead — see
 *     `./Features.tsx`'s Analytics section — because it was a genuine
 *     `centry.platform_config` flag, not a plugin descriptor.
 *   - **Service Descriptors** — a page of its own in this port, not yet done.
 *
 * ## What is left unavailable here is accurate
 *
 * #217 built this page with one live section, `resources`, and recorded that it
 * had put it here only because that is where the server's schema had it and
 * because leaving it out would have kept #26 open for another unit. Unit A14's
 * **Features** page has now taken it, which is where the reference puts it
 * (`ConfigurationPage.jsx` subtracts it via `MOVED_TO_FEATURES`;
 * `FeaturesPage.jsx` renders it as "Help Center"), together with the
 * `mcp_exposure.*` and `publishing_guardrail.*` fields that used to sit inside
 * Guardrails.
 *
 * What is left refusing is every section this platform cannot serve — a true
 * statement about this deployment, and a far more useful one than a form over
 * values nothing reads. A section leaves that group in one of two ways: it
 * acquires a consumer and its `unavailable_reason` is removed (Guardrails), or
 * its data needs a shape and a secret store this page cannot provide and it
 * acquires a `managed_surface` (MCP Servers, Authentication). Neither is a
 * change this file makes: both are declared by the server.
 *
 * The behaviour is shared with Features through `useAdminConfigSectionsPage`;
 * this file is layout and the page's identity.
 *
 * Rendering those as forms would be the worst version of this page's failure
 * mode: unlike an empty table, a configuration form that saves into a void
 * leaves the operator believing the setting took effect. Every one of them was
 * doing exactly that before this unit — the PUT never read its request body.
 *
 * ## What is deliberately absent
 *
 * The reference's **restart bar**. `requires_restart` names the pylons whose
 * plugins must reload; there are none, `plugin_config_restart` answers 501, and
 * the one writable section declares no field carrying the flag. A reload button
 * with nothing behind it is the control this unit exists to remove.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. Every route here is gated server-side on
 * `runtime.plugins`, the permission every pylon handler in this set declares,
 * and sections carrying a `required_permission` are checked again inside the
 * handler because that permission depends on the section.
 */
import type { ComponentType } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminEmailEditor } from './AdminEmailEditor';
import { AdminIdentityProvidersEditor } from './AdminIdentityProvidersEditor';
import { AdminLlmProxyEditor } from './AdminLlmProxyEditor';
import { AdminMcpServersEditor } from './AdminMcpServersEditor';
import { BrandingSectionCard } from './BrandingSectionCard';
import { EMAIL_MANAGED_SURFACE } from './api/adminEmailApi';
import { IDENTITY_PROVIDERS_MANAGED_SURFACE } from './api/adminIdentityProvidersApi';
import { LLM_PROXY_MANAGED_SURFACE } from './api/adminLlmProxyApi';
import { MCP_SERVERS_MANAGED_SURFACE } from './api/adminMcpServersApi';
import { ConfigurationSectionForm } from './ConfigurationSectionForm';
import { useAdminConfigurationPage, type AdminConfigurationPageState } from './useAdminConfigurationPage';

/**
 * The sections this app can render a DEDICATED editor for, keyed by the
 * server's `managed_surface`.
 *
 * One registry, in one place, keyed on the server's word rather than on a
 * section id. That is the correction #217 made when it moved
 * `service_descriptors`: the reference keeps section placement in two
 * client-side lists that must stay each other's complement by hand, and the
 * drift is invisible until a section renders on the wrong page or on both.
 *
 * A section whose `managed_surface` is NOT in this map falls through to its
 * `unavailable_reason`, so a surface this build does not know how to render
 * degrades to the honest explanation rather than to a blank pane.
 */
const MANAGED_SECTION_EDITORS: Readonly<Record<string, ComponentType>> = {
  [MCP_SERVERS_MANAGED_SURFACE]: AdminMcpServersEditor,
  [IDENTITY_PROVIDERS_MANAGED_SURFACE]: AdminIdentityProvidersEditor,
  [LLM_PROXY_MANAGED_SURFACE]: AdminLlmProxyEditor,
  [EMAIL_MANAGED_SURFACE]: AdminEmailEditor,
};

/** The dedicated editor for a section, when this build has one. */
/**
 * Sections whose dedicated page lives in THIS SPA, keyed on the section id.
 *
 * `MANAGED_SECTION_EDITORS` keys on the server's `managed_surface` word, and
 * that stays the rule for a section whose data lives on another surface. The
 * `branding` section is different: its rows ARE what the generic form would
 * edit, so the server declares no other surface for them — the reason not to
 * render the generic form here is that `/admin/app/branding` edits the same
 * rows as a brand pack (colour derivation, uploads, a preview) and two forms
 * over one row set would disagree the first time one of them cleaned a value
 * the other did not (ADR-0024 WP4). The id is the only handle there is.
 */
const SECTION_ID_EDITORS: Readonly<Record<string, ComponentType>> = {
  branding: BrandingSectionCard,
};

function managedEditorFor(
  section: { readonly id?: string; readonly managed_surface?: string } | undefined,
): ComponentType | undefined {
  const byId = section?.id === undefined ? undefined : SECTION_ID_EDITORS[section.id];
  if (byId !== undefined) return byId;
  const surface = section?.managed_surface;
  return surface === undefined ? undefined : MANAGED_SECTION_EDITORS[surface];
}

function SectionBody({ state }: { readonly state: AdminConfigurationPageState }) {
  if (state.activeSection === undefined) {
    return (
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.configuration.empty', 'This deployment publishes no configuration sections.')}
      </Typography>
    );
  }

  // A managed section is rendered by its own editor, BEFORE the unavailability
  // check. The section keeps its `unavailable_reason` — it is still true of the
  // plugin-config value endpoints, which cannot serve this data — so a build
  // that does not recognise the surface still explains itself.
  const ManagedEditor = managedEditorFor(state.activeSection);
  if (ManagedEditor !== undefined) {
    return <ManagedEditor />;
  }

  if (state.unavailableReason !== undefined) {
    return (
      <Alert severity="info" data-testid="admin-configuration-unavailable">
        {state.unavailableReason}
      </Alert>
    );
  }

  if (state.valuesError !== undefined) {
    return (
      <Alert severity="warning" data-testid="admin-configuration-values-error">
        {state.valuesError === 'load'
          ? t('pages.admin.configuration.error.load', 'Failed to load this section.')
          : state.valuesError}
      </Alert>
    );
  }

  if (state.isLoadingValues) {
    return (
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.configuration.loading', 'Loading configuration…')}
      </Typography>
    );
  }

  return <SectionForm state={state} />;
}

/**
 * The ordinary schema-driven form for an available section.
 *
 * Split out of `SectionBody` so that function stays a chain of early returns
 * over the states a section can be in — unavailable, managed, failed, loading,
 * ready — and the form itself is one thing. Both were over the complexity gate
 * as one function, and the gate was right: the branching was the part that had
 * grown, not the markup.
 */
function SectionForm({ state }: { readonly state: AdminConfigurationPageState }) {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {state.activeSection?.description !== undefined && state.activeSection.description !== '' ? (
        <Typography variant="bodySmall" color="text.secondary">
          {state.activeSection.description}
        </Typography>
      ) : null}
      <ConfigurationSectionForm
        fields={state.activeSection?.fields ?? []}
        values={state.values}
        disabled={state.isSaving}
        onChange={state.onFieldChange}
      />
      <Box sx={{ display: 'flex', gap: '0.5rem' }}>
        <Button
          size="small"
          variant="secondary"
          disabled={!state.isDirty || state.isSaving}
          onClick={state.onDiscard}
          sx={{ textTransform: 'none' }}
        >
          {t('pages.admin.configuration.discard', 'Discard')}
        </Button>
        <Button
          size="small"
          variant="elitea" color="primary"
          disabled={!state.isDirty || state.isSaving}
          onClick={state.onSave}
          sx={{ textTransform: 'none' }}
        >
          {state.isSaving
            ? t('pages.admin.configuration.saving', 'Saving…')
            : t('pages.admin.configuration.save', 'Save')}
        </Button>
      </Box>
    </Box>
  );
}

export function AdminConfiguration() {
  const state = useAdminConfigurationPage();

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Typography variant="h5" sx={{ fontWeight: 600 }}>
        {t('pages.admin.configuration.title', 'Configuration')}
      </Typography>

      {state.isLoadingSections ? <LinearProgress /> : null}

      {state.sectionsError !== undefined ? (
        <Alert severity="warning" data-testid="admin-configuration-sections-error">
          {state.sectionsError === 'load'
            ? t('pages.admin.configuration.error.sections', 'Failed to load the configuration sections.')
            : state.sectionsError}
        </Alert>
      ) : null}

      {state.saveError !== undefined ? (
        <Alert severity="error" onClose={state.onDismissError} data-testid="admin-configuration-error">
          {state.saveError === 'save'
            ? t('pages.admin.configuration.error.save', 'Failed to save this section.')
            : state.saveError}
        </Alert>
      ) : null}

      {state.savedAt !== undefined ? (
        <Alert severity="success" onClose={state.onDismissSaved} data-testid="admin-configuration-saved">
          {t('pages.admin.configuration.saved', 'Configuration saved.')}
        </Alert>
      ) : null}

      <Box sx={{ display: 'flex', gap: '1.5rem', alignItems: 'flex-start', flexWrap: 'wrap' }}>
        <List
          // `component="nav"`, not the default `<ul>`: `ListItemButton`
          // renders a `<div>`, and a `<ul>` whose children are divs is an axe
          // `list` violation (serious) — caught by journey 34's a11y check.
          component="nav"
          dense
          aria-label={t('pages.admin.configuration.sections', 'Configuration sections')}
          sx={{ width: '14rem', flex: '0 0 auto' }}
        >
          {state.sections.map((section) => (
            <ListItemButton
              key={section.id}
              selected={section.id === state.activeSection?.id}
              onClick={() => {
                state.onSelectSection(section.id);
              }}
            >
              <ListItemText
                primary={section.title}
                // The sidebar says which sections this deployment can serve
                // BEFORE the operator clicks one. Discovering it only on arrival
                // makes the page feel broken rather than scoped.
                // A section with a dedicated editor is NOT "not available
                // here": it is available, through its own surface. Labelling it
                // otherwise would send an operator away from the one page that
                // can edit it.
                secondary={
                  section.unavailable_reason === undefined ||
                  managedEditorFor(section) !== undefined
                    ? undefined
                    : t('pages.admin.configuration.sectionUnavailable', 'Not available here')
                }
              />
            </ListItemButton>
          ))}
        </List>
        <Box sx={{ flex: '1 1 24rem', minWidth: '18rem' }}>
          <SectionBody state={state} />
        </Box>
      </Box>
    </DrawerPage>
  );
}
