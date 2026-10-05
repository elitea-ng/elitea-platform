/**
 * Admin › Service Descriptors — unit A14, issue #200. The last of the ten.
 *
 * Reference (read-only): `apps/admin-ui/frontend/src/pages/ServiceDescriptorsPage/`
 * (353 lines across a page, a section and a table) plus `api/serviceDescriptorsApi.js`.
 *
 * ## This page is unavailable, and the reason comes from the server
 *
 * A "service descriptor" is not a description of this platform's services. It is
 * one registration in pylon's PROVIDER HUB: an external provider — DeepWiki,
 * Inventory, an image-generation service — that posts an
 * `ExternalServiceProviderDescriptor` naming its URL and the toolkits it offers,
 * which elitea_core stores, health-checks, and thereafter hands to agents through
 * `lookup_provider`. The four columns the reference table renders come from two
 * different places: `project_id`, `provider_name` and `service_location_url` are
 * the stored key, and `healthy` is which of two IN-PROCESS DICTS on the Pylon
 * plugin module the descriptor landed in when that process last started.
 *
 * This platform has no descriptor store, no provider health probe and no provider
 * lookup — the subsystem's only trace in the Go tree is a constant asserting its
 * absence. So there is nothing to list, and a registration would reach nothing.
 * The replacement is specified (ADR-0012 and the Provider Service Protocol) but
 * both are still `In Review`, sit in migration phase P3, and deliberately replace
 * this page's mutable descriptor with an immutable admitted manifest.
 *
 * The page therefore renders the SERVER's sentence, obtained by calling the
 * endpoint and being refused — it does not carry a copy of the reason. That
 * matters more here than anywhere else in this port: a page that hardcoded its
 * own explanation would keep displaying it after someone wired the endpoint to a
 * stub, which is exactly the regression this unit exists to prevent. If the
 * server ever answers 200 with rows, this page shows them.
 *
 * ## What this page can do now (migrations 0107 and 0109)
 *
 * The prose above describes the surface as it stood before the admission plane
 * existed, and the paragraph that used to follow it -- "there are no rows" --
 * has been removed rather than annotated, because a stale disclosure reads as
 * current and nothing fails when it stops being true. There is a store: the
 * listing answers from tables, registration records an immutable revision, and
 * migration 0109 adds the policy overlay that lets one be ACTIVATED.
 *
 * So this page carries two controls it did not have:
 *
 * **Activate**, on an inactive row, behind a dialog with a REQUIRED reason. Not
 * a `window.confirm`: an activation records an operator's sentence on the
 * revision row, and a confirm dialog cannot collect one. The request asserts the
 * row's own `published_manifest_digest`, so a provider that republished between
 * the review and the click is refused with 422 rather than activated against
 * bytes nobody read.
 *
 * **Deactivate**, on an active row. A separate verb from the reference's delete,
 * which REVOKES: revocation is terminal and records who and when, while
 * deactivating returns the revision to the state registration left it in.
 *
 * **The search box** is still absent. It filters a client-side array, and the
 * listing is one row per registered provider -- a handful on any deployment.
 *
 * ## The posture line
 *
 * `admission_posture` comes from the server (`ELITEA_PROVIDER_ADMISSION`) and is
 * rendered above the table, because `inactive` means two different things
 * without it: under `record` an inactive provider still serves every invoke, and
 * under `enforce` it is refused. Nothing is rendered when the server sent no
 * posture -- a page that guessed `record` would put a reassuring word on the
 * screen for a deployment that might be enforcing.
 *
 * ## One more thing worth recording about the reference
 *
 * `ServiceDescriptorsPage.jsx` — the file this page is named after — is itself
 * dead code in `admin_ui`: `routes.js` declares no route for it and nothing
 * imports it. The surface an operator could actually reach is the sibling
 * `ServiceDescriptorsSection`, embedded in the Configuration page. Both are
 * covered: this route exists so the page has a home, and the Configuration
 * section states the same sentence (the Go constant is shared, and a test pins
 * that they are byte-identical).
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. The listing is gated server-side on
 * `runtime.airun.serviceproviders` and the two registration verbs on
 * `provider_hub.descriptor.register`, the permissions the pylon originals
 * declare, resolved in `administration` mode. Activation is gated separately on
 * `provider_hub.descriptor.activate` (migration 0109): every facade registrar
 * files a registration at boot, so `.register` is handed out freely, while
 * activation is the switch that lets agents call the provider. The gate runs BEFORE the refusal,
 * so a caller without the permission sees an access error rather than learning
 * which subsystems this deployment runs.
 */
import { useCallback, useState } from 'react';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import {
  ServiceDescriptorDecisionDialog,
  type AdmissionDecision,
} from './ServiceDescriptorDecisionDialog';
import { AdminServiceDescriptorsTable } from './ServiceDescriptorsTable';
import {
  descriptorFailureReason,
  descriptorFailureStatus,
  useAdminServiceDescriptors,
  type AdminServiceDescriptor,
} from './api/adminServiceDescriptorsApi';

export function AdminServiceDescriptors() {
  const query = useAdminServiceDescriptors();

  // WHICH ROW is under decision lives here; WHAT IS SENT for it lives in the
  // dialog. The split is not tidiness — the page's own function tripped the
  // complexity gate with the dialog inline — but it is also the right seam:
  // the table produces a decision, the dialog consumes one.
  const [decision, setDecision] = useState<AdmissionDecision | null>(null);
  const onActivate = useCallback((descriptor: AdminServiceDescriptor) => {
    setDecision({ verb: 'activate', descriptor });
  }, []);
  const onDeactivate = useCallback((descriptor: AdminServiceDescriptor) => {
    setDecision({ verb: 'deactivate', descriptor });
  }, []);
  const closeDecision = useCallback(() => setDecision(null), []);

  const reason = descriptorFailureReason(query.error);
  const status = descriptorFailureStatus(query.error);
  // 501 is "this platform does not have that subsystem" and any other failure is
  // "the request went wrong". Collapsing them would let a 500 masquerade as a
  // considered architectural decision.
  const isUnavailable = status === 501;

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Typography variant="headingLarge" component="h1">
        {t('pages.admin.serviceDescriptors.title', 'Service Descriptors')}
      </Typography>

      {query.isPending ? <LinearProgress /> : null}

      {isUnavailable ? (
        <Alert severity="info" data-testid="admin-service-descriptors-unavailable">
          <AlertTitle>
            {t(
              'pages.admin.serviceDescriptors.unavailableTitle',
              'Provider registrations are not available on this platform',
            )}
          </AlertTitle>
          {/* The server's own sentence, not one this page carries. */}
          {reason}
        </Alert>
      ) : null}

      {query.isError && !isUnavailable ? (
        <Alert severity="warning" data-testid="admin-service-descriptors-error">
          {reason ??
            t(
              'pages.admin.serviceDescriptors.error.load',
              'Failed to load the registered service descriptors.',
            )}
        </Alert>
      ) : null}

      {/* THE POSTURE, above the table, because `inactive` means two different
          things without it. Nothing is rendered when the server sent none: a
          guessed `record` would be a reassuring word on an enforcing
          deployment. */}
      {query.isSuccess && query.data.posture !== undefined ? (
        <Typography variant="bodyMedium" component="p" color="text.secondary" data-testid="admin-admission-posture">
          {query.data.posture === 'enforce'
            ? t(
                'pages.admin.serviceDescriptors.posture.enforce',
                'Admission in force: a provider that is not active is refused.',
              )
            : t(
                'pages.admin.serviceDescriptors.posture.record',
                'Admission recorded only: a provider that is not active still serves requests.',
              )}
        </Typography>
      ) : null}

      {/* Rendered whenever the server answered. If a descriptor store ever
          lands, this page shows it without a further change here — which is
          what stops the explanation above from outliving its truth. */}
      {query.isSuccess ? (
        <AdminServiceDescriptorsTable
          descriptors={query.data.rows}
          onActivate={onActivate}
          onDeactivate={onDeactivate}
        />
      ) : null}

      <ServiceDescriptorDecisionDialog decision={decision} onClose={closeDecision} />
    </DrawerPage>
  );
}
