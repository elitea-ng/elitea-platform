/**
 * Confirmation for the three SCIM client actions that break a running
 * integration. Each says what it does to the identity provider BEFORE it does
 * it: a rotate or a revoke takes effect immediately, and provisioning fails
 * from that moment until the provider is reconfigured.
 */
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

import type { AdminScimClientAction } from "./AdminScimClientTable";
import type { AdminScimClient } from "./api/adminScimClientsApi";

export interface AdminScimClientPendingAction {
  readonly action: AdminScimClientAction;
  readonly client: AdminScimClient;
}

function copyFor(pending: AdminScimClientPendingAction): {
  title: string;
  body: string;
  confirm: string;
} {
  const name = pending.client.name;
  switch (pending.action) {
    case "rotate":
      return {
        title: t("pages.admin.scimClients.rotate.title", "Rotate secret"),
        body:
          pending.client.auth_method === "client_credentials"
            ? t(
                "pages.admin.scimClients.rotate.bodyClientCredentials",
                "A new client secret is issued for “{{name}}”. The old secret and every access token issued with it stop working immediately; update the identity provider with the new secret.",
                { name },
              )
            : t(
                "pages.admin.scimClients.rotate.body",
                "A new secret token is issued for “{{name}}”. The old token stops working immediately; update the identity provider with the new token.",
                { name },
              ),
        confirm: t("pages.admin.scimClients.rotate.confirm", "Rotate"),
      };
    case "revoke":
      return {
        title: t("pages.admin.scimClients.revoke.title", "Revoke SCIM client"),
        body: t(
          "pages.admin.scimClients.revoke.body",
          "“{{name}}” stops authenticating immediately, and the identity provider using it can no longer provision users or groups. The record stays listed as revoked.",
          { name },
        ),
        confirm: t("pages.admin.scimClients.revoke.confirm", "Revoke"),
      };
    case "delete":
      return {
        title: t("pages.admin.scimClients.delete.title", "Delete SCIM client"),
        body: t(
          "pages.admin.scimClients.delete.body",
          "This removes the record of “{{name}}”. If it is still active, the identity provider using it can no longer provision.",
          { name },
        ),
        confirm: t("pages.admin.scimClients.delete.confirm", "Delete"),
      };
  }
}

export interface AdminScimClientConfirmDialogProps {
  readonly pending: AdminScimClientPendingAction | undefined;
  readonly busy: boolean;
  readonly onCancel: () => void;
  readonly onConfirm: () => void;
}

export function AdminScimClientConfirmDialog({
  pending,
  busy,
  onCancel,
  onConfirm,
}: AdminScimClientConfirmDialogProps) {
  const copy = pending === undefined ? undefined : copyFor(pending);
  return (
    <Dialog
      open={pending !== undefined}
      onClose={onCancel}
      maxWidth="xs"
      fullWidth
      aria-labelledby="admin-scim-client-confirm-title"
      data-testid="admin-scim-client-confirm-dialog"
    >
      <DialogTitle id="admin-scim-client-confirm-title">
        {copy?.title}
      </DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium">{copy?.body}</Typography>
      </DialogContent>
      <DialogActions>
        <Button
          onClick={onCancel}
          disabled={busy}
          sx={{ textTransform: "none" }}
        >
          {t("pages.admin.scimClients.cancel", "Cancel")}
        </Button>
        <Button
          variant="elitea"
          color="alarm"
          onClick={onConfirm}
          disabled={busy}
          sx={{ textTransform: "none" }}
          data-testid="admin-scim-client-confirm"
        >
          {copy?.confirm}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
