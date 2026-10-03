/**
 * Admin › Features — unit A14, issue #200.
 *
 * Reference (read-only): `apps/admin-ui/frontend/src/pages/FeaturesPage/FeaturesPage.jsx`
 * (496 lines) plus the four `SchemaForm` components it mounts — `GuardrailsSection`,
 * `HelpCenterSection`, `SupportAssistant`, `VoiceFeatures` (≈900 lines more).
 * Rewritten against this app's stack, as every page in this port has been.
 *
 * ## What this page is, and why it is thin
 *
 * The reference Features page is Configuration's twin: the same schema, the same
 * value endpoints, the same save, the same state machine — over a DIFFERENT
 * subset of sections. It keeps that subset in the client, as a six-entry array
 * plus three config-path prefixes, and Configuration keeps the complement, as
 * `MOVED_TO_FEATURES` plus the same three prefixes to subtract. Two hand-written
 * lists that must remain each other's exact complement, in two files, is a drift
 * waiting to happen: a section added on the server appears on Configuration, on
 * both, or on neither, depending on which list somebody remembered.
 *
 * Here the section says which page it belongs to (`page: "features"` in
 * `config_schemas.go`), both pages filter on it, and the whole of the behaviour —
 * section list, values query, delta-only save, server-declared unavailability —
 * is `useAdminConfigSectionsPage`, shared with Configuration. This file is the
 * layout and the six sections' identity; there is no second state machine.
 *
 * ## Which of the six actually do something
 *
 * Availability is declared by the SERVER and rendered here, never decided here.
 * As of this unit:
 *
 *   - **MCP Configuration** — live. `mcp_enabled` is marshalled into
 *     `GET /elitea_core/platform_settings/…` (where this app's four
 *     `useIsMcpVisible` hooks and its `/mcps` route read it) AND enforced as a
 *     403 on the three MCP proxy/sync routes, which is what the field's own
 *     description promises. `mcp_in_menu` arrives as `mcp_in_menu_enabled`, the
 *     key every one of those hooks documents as missing from the wire.
 *   - **Agent Publishing** — live. The block switch and the whitelist are
 *     enforced in the publish handler, which never consulted them before; the
 *     categories are merged into the Agents Hub's own read. One field inside it,
 *     `publish_validation_rules`, carries its own reason: publish validation in
 *     this service is deterministic and has no evaluator for a custom prompt to
 *     reach.
 *   - **Help Center** — live, and MOVED here from Configuration. #217 built the
 *     `resources` section over `centry.platform_config` and wired `/help-center`
 *     to read it back, closing #26, and recorded in its own report that the
 *     section belonged on this page. The reference agrees:
 *     `ConfigurationPage.jsx` subtracts `resources` via `MOVED_TO_FEATURES`.
 *     The Help Center's own read is unaffected — it calls a separate public
 *     route that has no notion of which admin page authored the row — and a Go
 *     test proves that rather than asserting it.
 *   - **Skill Publishing** — live, and the last of the three to become so. It was
 *     unavailable because the subsystem was absent: no publish endpoint, no
 *     catalog, no categories route. `internal/api/v2/skillpublish` built all
 *     three, and this section is the half that was still missing — the skill
 *     guardrail was being enforced against the AGENT section's switch for want
 *     of anywhere to author its own, and the catalog's category list was nine
 *     hardcoded defaults. The block switch and whitelist are enforced in
 *     `publish_skill`/`publish_skill_validate`; the categories are merged into
 *     `GET /elitea_core/skill_categories/…`, which the publish dialog and the
 *     public-catalog filter both read. One field inside it,
 *     `skill_publish_validation_rules`, carries its own reason, exactly as the
 *     agent section's does and for the same cause.
 *   - **Support Assistant** — live, and it is the section whose reason this
 *     unit's own doc comment used to hold up as the purest example of a flag
 *     nothing reads. Both halves of that reason are now false:
 *     `internal/api/v2/supportassistant` serves config, conversations and one
 *     agent turn per question off these very rows, and `widgets/app-shell`
 *     mounts `SupportAssistantWidget`, whose source is ported under
 *     `widgets/support-assistant/vendor/` rather than taken as a dependency —
 *     the published package streams over socket.io and this platform serves SSE.
 *   - **Voice Features** — unavailable, same shape: `VoiceControlButton` and
 *     `VoiceMiniPlayer` are exported from `features/chat-input` and imported by
 *     nothing, and the button hardcodes both flags as module constants.
 *   - **Analytics** — live, and the seventh section, not one of the reference's
 *     six. It arrived by MOVING here rather than by closing a gap: it used to
 *     be a field (`analytics_enabled`) on Configuration's withheld
 *     `observability` section, which this platform removed rather than ported
 *     (see `config_schemas.go`'s "Observability, Runtime and Admin Panel are
 *     GONE too" note) because none of that section's OTHER fields had anywhere
 *     to go. This one did: it is an ordinary `centry.platform_config` flag,
 *     read into `GET /elitea_core/platform_settings/…` as `analytics_enabled`
 *     and enforced server-side on every `/analytics*` route by
 *     `internal/api/router.go`'s `requireAnalyticsEnabled`.
 *
 * Rendering the one section that is still withheld as a live switch is the
 * failure this unit exists to remove. A feature flag nothing reads is its purest
 * form: unlike an empty table, it leaves the operator believing a platform-wide
 * switch was thrown.
 *
 * SIX OF THE SEVEN ARE NOW LIVE, which is worth noting because this page began
 * as the opposite: three of the reference's six were withheld, and the count
 * has only ever gone one way. Availability is still declared by the SERVER and
 * rendered here; nothing on this page decides it.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. Every route this page calls is gated server-side on
 * `runtime.plugins`.
 */
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

import { ConfigurationSectionForm } from './ConfigurationSectionForm';
import { useAdminFeaturesPage, type AdminConfigurationPageState } from './useAdminConfigurationPage';


function FeaturesSectionBody({ state }: { readonly state: AdminConfigurationPageState }) {
  if (state.activeSection === undefined) {
    return (
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.features.empty', 'This deployment publishes no feature sections.')}
      </Typography>
    );
  }

  if (state.unavailableReason !== undefined) {
    return (
      <Alert severity="info" data-testid="admin-features-unavailable">
        {state.unavailableReason}
      </Alert>
    );
  }

  if (state.valuesError !== undefined) {
    return (
      <Alert severity="warning" data-testid="admin-features-values-error">
        {state.valuesError === 'load'
          ? t('pages.admin.features.error.load', 'Failed to load this section.')
          : state.valuesError}
      </Alert>
    );
  }

  if (state.isLoadingValues) {
    return (
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.features.loading', 'Loading feature settings…')}
      </Typography>
    );
  }

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {state.activeSection.description !== undefined && state.activeSection.description !== '' ? (
        <Typography variant="bodySmall" color="text.secondary">
          {state.activeSection.description}
        </Typography>
      ) : null}
      <ConfigurationSectionForm
        fields={state.activeSection.fields ?? []}
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
          {t('pages.admin.features.discard', 'Discard')}
        </Button>
        <Button
          size="small"
          variant="elitea" color="primary"
          disabled={!state.isDirty || state.isSaving}
          onClick={state.onSave}
          sx={{ textTransform: 'none' }}
        >
          {state.isSaving
            ? t('pages.admin.features.saving', 'Saving…')
            : t('pages.admin.features.save', 'Save')}
        </Button>
      </Box>
    </Box>
  );
}

export function AdminFeatures() {
  const state = useAdminFeaturesPage();

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Typography variant="headingLarge" component="h1">
        {t('pages.admin.features.title', 'Features')}
      </Typography>

      {state.isLoadingSections ? <LinearProgress /> : null}

      {state.sectionsError !== undefined ? (
        <Alert severity="warning" data-testid="admin-features-sections-error">
          {state.sectionsError === 'load'
            ? t('pages.admin.features.error.sections', 'Failed to load the feature sections.')
            : state.sectionsError}
        </Alert>
      ) : null}

      {state.saveError !== undefined ? (
        <Alert severity="error" onClose={state.onDismissError} data-testid="admin-features-error">
          {state.saveError === 'save'
            ? t('pages.admin.features.error.save', 'Failed to save this section.')
            : state.saveError}
        </Alert>
      ) : null}

      {state.savedAt !== undefined ? (
        <Alert severity="success" onClose={state.onDismissSaved} data-testid="admin-features-saved">
          {t('pages.admin.features.saved', 'Feature settings saved.')}
        </Alert>
      ) : null}

      <Box sx={{ display: 'flex', gap: '1.5rem', alignItems: 'flex-start', flexWrap: 'wrap' }}>
        <List
          // `component="nav"`, not the default `<ul>`: `ListItemButton` renders a
          // `<div>`, and a `<ul>` whose children are divs is an axe `list`
          // violation (serious) — the same one journey 34 caught on the
          // Configuration page.
          component="nav"
          dense
          aria-label={t('pages.admin.features.sections', 'Feature sections')}
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
                // Half of this page's sections are unavailable on this platform.
                // Saying so in the sidebar means an operator learns the shape of
                // what is offered before clicking, rather than discovering it one
                // pane at a time.
                secondary={
                  section.unavailable_reason === undefined
                    ? undefined
                    : t('pages.admin.features.sectionUnavailable', 'Not available here')
                }
              />
            </ListItemButton>
          ))}
        </List>
        <Box sx={{ flex: '1 1 24rem', minWidth: '18rem' }}>
          <FeaturesSectionBody state={state} />
        </Box>
      </Box>
    </DrawerPage>
  );
}
