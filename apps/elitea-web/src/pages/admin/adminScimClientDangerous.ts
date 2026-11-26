/**
 * What the dangerous-action dialog (`AdminDangerousActionDialog`, ADR-0059
 * R3.8b) says for the four SCIM client actions. Creating and rotating are
 * critical trust actions; a revoke and a delete are dangerous. Each says what
 * it does to the identity provider BEFORE it does it: a rotate or a revoke
 * takes effect immediately, and provisioning fails from that moment until the
 * provider is reconfigured.
 */
import { t } from "@/shared/i18n";

import type { AdminDangerousActionDialogProps } from "./AdminDangerousActionDialog";
import type { AdminScimClientAction } from "./AdminScimClientTable";
import type { AdminScimClient, AdminScimClientDraft } from "./api/adminScimClientsApi";

export type AdminScimClientPendingAction =
  | { readonly action: AdminScimClientAction; readonly client: AdminScimClient }
  | { readonly action: "create"; readonly draft: AdminScimClientDraft };

type ScimClientDialog = Pick<
  AdminDangerousActionDialogProps,
  "action" | "title" | "change" | "reach" | "target" | "confirmLabel"
>;

function rotateChange(client: AdminScimClient): string {
  const name = client.name;
  return client.auth_method === "client_credentials"
    ? t(
        "pages.admin.scimClients.rotate.bodyClientCredentials",
        "A new client secret is issued for “{{name}}”. The old secret and every access token issued with it stop working immediately; update the identity provider with the new secret.",
        { name },
      )
    : t(
        "pages.admin.scimClients.rotate.body",
        "A new secret token is issued for “{{name}}”. The old token stops working immediately; update the identity provider with the new token.",
        { name },
      );
}

/** The dialog's copy and action key for a pending SCIM client action. */
export function scimClientDialog(pending: AdminScimClientPendingAction): ScimClientDialog {
  // The people an identity provider provisions are not known here.
  const reach = { count: undefined, unit: "people" } as const;
  switch (pending.action) {
    case "create":
      return {
        action: "POST /api/v2/admin/scim_clients/administration",
        title: t("pages.admin.scimClients.createConfirm.title", "Create SCIM client"),
        change: t(
          "pages.admin.scimClients.createConfirm.body",
          "A new credential “{{name}}” can provision users and groups into the platform from the moment it is created.",
          { name: pending.draft.name },
        ),
        reach,
        target: pending.draft.name,
        confirmLabel: t("pages.admin.scimClients.createConfirm.confirm", "Create"),
      };
    case "rotate":
      return {
        action: "POST /api/v2/admin/scim_clients/administration/{id}/rotate",
        title: t("pages.admin.scimClients.rotate.title", "Rotate secret"),
        change: rotateChange(pending.client),
        reach,
        target: pending.client.name,
        confirmLabel: t("pages.admin.scimClients.rotate.confirm", "Rotate"),
      };
    case "revoke":
      return {
        action: "POST /api/v2/admin/scim_clients/administration/{id}/revoke",
        title: t("pages.admin.scimClients.revoke.title", "Revoke SCIM client"),
        change: t(
          "pages.admin.scimClients.revoke.body",
          "“{{name}}” stops authenticating immediately, and the identity provider using it can no longer provision users or groups. The record stays listed as revoked.",
          { name: pending.client.name },
        ),
        reach,
        target: pending.client.name,
        confirmLabel: t("pages.admin.scimClients.revoke.confirm", "Revoke"),
      };
    case "delete":
      return {
        action: "DELETE /api/v2/admin/scim_clients/administration/{id}",
        title: t("pages.admin.scimClients.delete.title", "Delete SCIM client"),
        change: t(
          "pages.admin.scimClients.delete.body",
          "This removes the record of “{{name}}”. If it is still active, the identity provider using it can no longer provision.",
          { name: pending.client.name },
        ),
        reach,
        target: pending.client.name,
        confirmLabel: t("pages.admin.scimClients.delete.confirm", "Delete"),
      };
  }
}
