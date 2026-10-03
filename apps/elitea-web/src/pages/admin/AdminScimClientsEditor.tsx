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

import { AdminScimClientConfirmDialog } from "./AdminScimClientConfirmDialog";
import type { AdminScimClientPendingAction } from "./AdminScimClientConfirmDialog";
import { AdminScimClientCreateDialog } from "./AdminScimClientCreateDialog";
import { AdminScimClientSecretDialog } from "./AdminScimClientSecretDialog";
import { AdminScimClientTable } from "./AdminScimClientTable";
import { AdminScimCopyField } from "./AdminScimCopyField";
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

function loadErrorMessage(error: unknown): string {
  if (configFailureStatus(error) === 403) {
    return t(
      "pages.admin.scimClients.error.forbidden",
      "You need the admin.auth.users permission to manage SCIM clients.",
    );
  }
  return (
    configFailureReason(error) ??
    t("pages.admin.scimClients.error.load", "Failed to load the SCIM clients.")
  );
}

function actionFailure(
  error: unknown,
  action: AdminScimClientPendingAction["action"],
): string {
  const fallback = {
    rotate: t("pages.admin.scimClients.error.rotate", "Failed to rotate that SCIM client's secret."),
    revoke: t("pages.admin.scimClients.error.revoke", "Failed to revoke that SCIM client."),
    delete: t("pages.admin.scimClients.error.delete", "Failed to delete that SCIM client."),
  }[action];
  return configFailureReason(error) ?? fallback;
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
  const listQuery = useAdminScimClients();
  const createMutation = useCreateAdminScimClient();
  const rotateMutation = useRotateAdminScimClient();
  const revokeMutation = useRevokeAdminScimClient();
  const deleteMutation = useDeleteAdminScimClient();

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
  const busy =
    rotateMutation.isPending ||
    revokeMutation.isPending ||
    deleteMutation.isPending;

  const handleCreate = (draft: AdminScimClientDraft): void => {
    setCreateError(undefined);
    createMutation.mutate(draft, {
      onSuccess: (result) => {
        setCreateOpen(false);
        setReveal(result);
      },
      onError: (error: unknown) => {
        setCreateError(
          configFailureReason(error) ??
            t(
              "pages.admin.scimClients.error.create",
              "Failed to create that SCIM client.",
            ),
        );
      },
    });
  };

  const handleConfirm = (): void => {
    if (pending === undefined) return;
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
      rotateMutation.mutate(client.id, {
        onSuccess: (result) => {
          setPending(undefined);
          setReveal(result);
        },
        onError,
      });
      return;
    }
    const mutation = action === "revoke" ? revokeMutation : deleteMutation;
    mutation.mutate(client.id, { onSuccess: onDone, onError });
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
        busy={busy}
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
        onSubmit={handleCreate}
      />

      <AdminScimClientConfirmDialog
        pending={pending}
        busy={busy}
        onCancel={() => {
          setPending(undefined);
        }}
        onConfirm={handleConfirm}
      />

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
