/**
 * Admin › Configuration › Authentication — SCIM provisioning credentials.
 *
 * SCIM 2.0 accepts only a credential minted for it here; a personal access
 * token is refused. It renders BEFORE the group bindings: an identity provider
 * needs a credential before any group it pushes can be bound.
 *
 * ## The secret lives in component state only
 *
 * A create or a rotate answers the plaintext once. It is held in `reveal` for
 * the one-time dialog and dropped when the dialog closes; the list query is
 * invalidated rather than written from the response, so no cache entry ever
 * holds it.
 */
import { useMemo, useState } from "react";

import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import LinearProgress from "@mui/material/LinearProgress";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

import { AdminIdentityTrustNotice, useIdentityTrustLocked } from "./AdminIdentityTrustNotice";
import { AdminDangerousActionDialog } from "./AdminDangerousActionDialog";
import type { AdminReason } from "./adminDangerousAction";
import { scimClientDialog, type AdminScimClientPendingAction } from "./adminScimClientDangerous";
import { AdminScimClientCreateDialog } from "./AdminScimClientCreateDialog";
import { AdminScimClientSecretDialog } from "./AdminScimClientSecretDialog";
import { AdminScimClientTable, type AdminScimClientAction } from "./AdminScimClientTable";
import { AdminScimCopyField } from "./AdminScimCopyField";
import { ADMIN_ITEM } from "./adminItemIds";
import { useAdminWritable } from "./adminWriteAccess";
import {
  configFailureReason,
  configFailureStatus,
} from "./api/adminConfigurationApi";
import {
  SCIM_BASE_PATH,
  SCIM_TOKEN_ENDPOINT_PATH,
  useAdminScimClients,
  useCreateAdminScimClient,
  useDeleteAdminScimClient,
  useRevokeAdminScimClient,
  useRotateAdminScimClient,
  type AdminScimClientDraft,
  type AdminScimClientSecret,
} from "./api/adminScimClientsApi";
import { identityTrustFailureReason } from "./api/identityTrustRefusal";

function loadErrorMessage(error: unknown): string {
  if (configFailureStatus(error) === 403) {
    return t(
      "pages.admin.scimClients.error.forbidden",
      "You need the identity.sso.view or admin.auth.users permission to see SCIM clients.",
    );
  }
  return (
    configFailureReason(error) ??
    t("pages.admin.scimClients.error.load", "Failed to load the SCIM clients.")
  );
}

function actionFailure(
  error: unknown,
  action: AdminScimClientAction,
): string {
  const fallback = {
    rotate: t("pages.admin.scimClients.error.rotate", "Failed to rotate that SCIM client's secret."),
    revoke: t("pages.admin.scimClients.error.revoke", "Failed to revoke that SCIM client."),
    delete: t("pages.admin.scimClients.error.delete", "Failed to delete that SCIM client."),
  }[action];
  return identityTrustFailureReason(error) ?? fallback;
}

/** Absolute URLs an identity provider is configured with, for this origin. */
function connectionUrls(
  list: { scimBasePath: string; tokenEndpointPath: string } | undefined,
): { tenantUrl: string; tokenEndpoint: string } {
  const origin = window.location.origin;
  return {
    tenantUrl: `${origin}${list?.scimBasePath ?? SCIM_BASE_PATH}`,
    tokenEndpoint: `${origin}${list?.tokenEndpointPath ?? SCIM_TOKEN_ENDPOINT_PATH}`,
  };
}

export function AdminScimClientsEditor() {
  const access = useAdminWritable(ADMIN_ITEM.scim);
  const trustLocked = useIdentityTrustLocked();
  const listQuery = useAdminScimClients();
  const createMutation = access.guard(useCreateAdminScimClient());
  const rotateMutation = access.guard(useRotateAdminScimClient());
  const revokeMutation = access.guard(useRevokeAdminScimClient());
  const deleteMutation = access.guard(useDeleteAdminScimClient());

  const [createOpen, setCreateOpen] = useState(false);
  const [createError, setCreateError] = useState<string | undefined>(undefined);
  const [pending, setPending] = useState<
    AdminScimClientPendingAction | undefined
  >(undefined);
  const [actionError, setActionError] = useState<string | undefined>(undefined);
  const [reveal, setReveal] = useState<AdminScimClientSecret | undefined>(
    undefined,
  );

  const list = listQuery.data;
  const clients = useMemo(() => list?.clients ?? [], [list]);
  const { tenantUrl, tokenEndpoint } = connectionUrls(list);
  const busy = [rotateMutation, revokeMutation, deleteMutation].some(
    (mutation) => mutation.isPending,
  );

  const handleCreate = (draft: AdminScimClientDraft, reason: AdminReason): void => {
    setCreateError(undefined);
    createMutation.mutate({ draft, reason }, {
      onSuccess: (result) => {
        setCreateOpen(false);
        setReveal(result);
      },
      onError: (error: unknown) => {
        setCreateError(
          identityTrustFailureReason(error) ??
            t(
              "pages.admin.scimClients.error.create",
              "Failed to create that SCIM client.",
            ),
        );
      },
    });
  };

  const handleConfirm = (reason: AdminReason): void => {
    if (pending === undefined) return;
    if (pending.action === "create") {
      setPending(undefined);
      handleCreate(pending.draft, reason);
      return;
    }
    const { action, client } = pending;
    setActionError(undefined);
    const onError = (error: unknown): void => {
      setActionError(actionFailure(error, action));
      setPending(undefined);
    };
    const onDone = (): void => {
      setPending(undefined);
    };
    if (action === "rotate") {
      rotateMutation.mutate({ id: client.id, reason }, {
        onSuccess: (result) => {
          setPending(undefined);
          setReveal(result);
        },
        onError,
      });
      return;
    }
    const mutation = action === "revoke" ? revokeMutation : deleteMutation;
    mutation.mutate({ id: client.id, reason }, { onSuccess: onDone, onError });
  };

  return (
    <Box
      sx={{ display: "flex", flexDirection: "column", gap: "1rem" }}
      data-testid="admin-scim-clients"
    >
      <Typography variant="headingSmall" component="h2">
        {t("pages.admin.scimClients.title", "SCIM provisioning")}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          "pages.admin.scimClients.description",
          "An identity provider such as Microsoft Entra ID provisions users and groups with a dedicated SCIM client credential created here. Personal access tokens are no longer accepted by the SCIM endpoint.",
        )}
      </Typography>

      <Stack spacing={2} sx={{ maxWidth: "40rem" }}>
        <AdminScimCopyField
          label={t("pages.admin.scimClients.tenantUrl", "Tenant URL")}
          value={tenantUrl}
          testId="admin-scim-clients-tenant-url"
        />
        <AdminScimCopyField
          label={t("pages.admin.scimClients.tokenEndpoint", "Token endpoint")}
          value={tokenEndpoint}
          testId="admin-scim-clients-token-endpoint"
        />
      </Stack>

      <AdminIdentityTrustNotice subject="scim" />

      {listQuery.isLoading ? <LinearProgress /> : null}

      {listQuery.error != null ? (
        <Alert severity="warning" data-testid="admin-scim-clients-error">
          {loadErrorMessage(listQuery.error)}
        </Alert>
      ) : null}

      {actionError !== undefined ? (
        <Alert
          severity="error"
          onClose={() => {
            setActionError(undefined);
          }}
          data-testid="admin-scim-clients-action-error"
        >
          {actionError}
        </Alert>
      ) : null}

      <Box>
        <Button
          size="small"
          variant="elitea"
          color="primary"
          onClick={() => {
            setCreateError(undefined);
            setCreateOpen(true);
          }}
          // Creating a SCIM client is a trust action (ADR-0059 R0.4c).
          disabled={!access.writable || trustLocked}
          sx={{ textTransform: "none" }}
          data-testid="admin-scim-clients-add"
        >
          {t("pages.admin.scimClients.add", "New SCIM client")}
        </Button>
      </Box>

      {!listQuery.isLoading &&
      clients.length === 0 &&
      listQuery.error == null ? (
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          data-testid="admin-scim-clients-empty"
        >
          {t(
            "pages.admin.scimClients.empty",
            "No SCIM client exists. An identity provider cannot provision until one is created.",
          )}
        </Typography>
      ) : null}

      <AdminScimClientTable
        clients={clients}
        busy={busy || !access.writable}
        onAction={(action, client) => {
          setPending({ action, client });
        }}
      />

      <AdminScimClientCreateDialog
        open={createOpen}
        isSaving={createMutation.isPending}
        serverError={createError}
        onClose={() => {
          setCreateOpen(false);
        }}
        onSubmit={(draft) => {
          setPending({ action: "create", draft });
        }}
      />

      {pending !== undefined ? (
        <AdminDangerousActionDialog
          open
          {...scimClientDialog(pending)}
          pending={busy || createMutation.isPending}
          onClose={() => {
            setPending(undefined);
          }}
          onConfirm={handleConfirm}
        />
      ) : null}

      <AdminScimClientSecretDialog
        reveal={reveal}
        tenantUrl={tenantUrl}
        tokenEndpoint={tokenEndpoint}
        onClose={() => {
          setReveal(undefined);
          // The mutation keeps its last result too; drop it with the dialog.
          createMutation.reset();
          rotateMutation.reset();
        }}
      />
    </Box>
  );
}
