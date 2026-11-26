/**
 * What the dangerous-action dialog (`AdminDangerousActionDialog`, ADR-0059
 * R3.8b) says for an identity provider save or delete. Every provider write
 * is a critical trust action: the server tells a trust change, a disable and
 * a settings edit apart, so every save carries the reason.
 */
import { t } from "@/shared/i18n";

import type { AdminDangerousActionDialogProps } from "./AdminDangerousActionDialog";
import type { AdminIdentityProvider, AdminIdentityProviderDraft } from "./api/adminIdentityProvidersApi";

export type PendingProviderAction =
  | { readonly kind: "save"; readonly draft: AdminIdentityProviderDraft }
  | { readonly kind: "delete"; readonly provider: AdminIdentityProvider };

/** The dangerous-action dialog's copy (ADR-0059 R3.8b): every provider write is a critical trust action. */
export function providerDialog(
  pending: PendingProviderAction,
): Pick<AdminDangerousActionDialogProps, "action" | "title" | "change" | "reach" | "target" | "confirmLabel"> {
  const reach = { count: undefined, unit: "people" } as const;
  if (pending.kind === "delete") {
    return {
      action: "DELETE /api/v2/admin/identity_providers/administration/{key}",
      title: t("pages.admin.identityProviders.delete.title", "Remove identity provider"),
      change: deleteWarning(pending.provider.display_name),
      reach,
      target: pending.provider.key,
      confirmLabel: t("pages.admin.identityProviders.delete.confirm", "Remove"),
    };
  }
  return {
    action: pending.draft.enabled
      ? "PUT /api/v2/admin/identity_providers/administration/{key} trust"
      : "PUT /api/v2/admin/identity_providers/administration/{key} disable",
    title: t("pages.admin.identityProviders.saveConfirm.title", "Save identity provider"),
    change: pending.draft.enabled
      ? t(
          "pages.admin.identityProviders.saveConfirm.body",
          "Logins through “{{name}}” follow the saved definition from the next sign-in. A changed issuer, endpoint, certificate or secret decides who the platform trusts.",
          { name: pending.draft.displayName },
        )
      : t(
          "pages.admin.identityProviders.saveConfirm.disableBody",
          "Nobody can sign in through “{{name}}” after this save until it is turned on again.",
          { name: pending.draft.displayName },
        ),
    reach,
    target: pending.draft.key,
    confirmLabel: t("pages.admin.identityProviders.saveConfirm.confirm", "Save"),
  };
}

/**
 * The delete confirmation's wording.
 *
 * Interpolated through `t`'s own options rather than by `String.replace` on the
 * fallback: the fallback is only used when the bundle has no entry, so a
 * `replace` would silently stop substituting the moment the string was
 * translated.
 */
function deleteWarning(name: string): string {
  return t(
    "pages.admin.identityProviders.delete.body",
    "This removes “{{name}}” and deletes its stored secret, which cannot be recovered. To stop using it while keeping the secret, edit it and clear “Use this provider for logins” instead.",
    { name },
  );
}

/** A delete is a trust action: a locked holder cannot confirm it; a save of the settings half still can. */
export function confirmLocked(pending: PendingProviderAction, locked: boolean): boolean {
  return pending.kind === "delete" && locked;
}
