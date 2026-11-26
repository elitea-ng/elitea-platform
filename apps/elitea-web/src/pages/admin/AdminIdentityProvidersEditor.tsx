/**
 * Admin › People and access › Identity providers — the identity provider
 * editor, on its own page (`./IdentityProviders.tsx`) since R3.6 (issue
 * 2537). The Configuration page's Authentication section links there.
 *
 * This is the section that used to render only a refusal about the Arbiter bus.
 * The refusal was accurate about the plugin-config VALUE endpoints, which still
 * cannot serve this section — two of its fields are credentials — and stopped
 * being the whole truth when federation acquired a store and a reader
 * (`./api/adminIdentityProvidersApi`, `elitea_auth.identity_providers`).
 *
 * A holder of the settings half alone (`identity.sso.settings.manage`, or
 * `admin`) sees the trust controls read-only, with the notice that a global
 * administrator changes them (`./AdminIdentityTrustNotice.tsx`).
 *
 * ## What an operator can and cannot see
 *
 * A stored secret renders as the mask the server sent. There is no "reveal":
 * the plaintext is sealed in the platform vault's hidden bucket and no endpoint
 * returns it, so there is nothing to reveal and a control offering to would be
 * a lie.
 *
 * ## One live provider per protocol, and the page says so
 *
 * Enabling a provider retires the other of its kind, in one transaction on the
 * server. That is not a limitation of this screen: the router mounts one route
 * set per protocol, so a second live definition of the same kind could never be
 * reached. The list shows which one is live.
 *
 * ## Delete confirms, because it destroys a credential
 *
 * Removing a provider also removes its sealed secret, which cannot be recovered
 * from this screen or any other. An operator who only wants to stop using it
 * should clear "Use this provider for logins" instead, which keeps the secret —
 * the confirmation says so.
 */
import { useMemo, useState } from "react";

import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import LinearProgress from "@mui/material/LinearProgress";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

import { AdminDangerousActionDialog } from "./AdminDangerousActionDialog";
import type { AdminReason } from "./adminDangerousAction";
import { confirmBlocked, providerDialog, type PendingProviderAction } from "./adminIdentityProviderDangerous";
import { AdminIdentityProviderDialog } from "./AdminIdentityProviderDialog";
import { AdminIdentityTrustNotice, useIdentityTrustLocked } from "./AdminIdentityTrustNotice";
import { ProviderTable } from "./AdminIdentityProviderTable";
import { AdminViewOnlyNotice, AdminWritable } from "./AdminWritable";
import { ADMIN_ITEM } from "./adminItemIds";
import { useAdminWritable } from "./adminWriteAccess";
import { configFailureReason } from "./api/adminConfigurationApi";
import { identityTrustFailureReason } from "./api/identityTrustRefusal";
import {
  useAdminIdentityProviders,
  useDeleteAdminIdentityProvider,
  useSaveAdminIdentityProvider,
  type AdminIdentityProvider,
  type AdminIdentityProviderDraft,
} from "./api/adminIdentityProvidersApi";

/**
 * The load and action alerts.
 *
 * Both render the SERVER's own sentence when it gave one. On this surface the
 * refusals name the field they refused ("the URL must use https: a plaintext
 * federation endpoint exposes the login"), and collapsing them into "Failed"
 * would discard the only words that say which value to fix.
 */
function ProviderAlerts({
  loadError,
  actionError,
  onDismissAction,
  rebindIssuer,
  onDismissRebind,
}: {
  readonly loadError: unknown;
  readonly actionError: string | undefined;
  readonly onDismissAction: () => void;
  readonly rebindIssuer: string | undefined;
  readonly onDismissRebind: () => void;
}) {
  return (
    <>
      {/* After an issuer change the existing accounts are still bound to the
          old issuer (issue 2558): say so at once, before every user's next
          sign-in is refused with no hint of why. */}
      {rebindIssuer !== undefined ? (
        <Alert
          severity="warning"
          onClose={onDismissRebind}
          data-testid="admin-identity-providers-rebind-note"
        >
          {t(
            "pages.admin.identityProviders.rebindNote",
            "The existing accounts are still bound to the previous issuer {{issuer}}, so they cannot sign in through the new one. If the identity provider really moved, a super admin re-binds them: see Authentication, Rotate an issuer, in the documentation.",
            { issuer: rebindIssuer },
          )}
        </Alert>
      ) : null}

      {loadError != null ? (
        <Alert severity="warning" data-testid="admin-identity-providers-error">
          {configFailureReason(loadError) ??
            t(
              "pages.admin.identityProviders.error.load",
              "Failed to load the identity providers.",
            )}
        </Alert>
      ) : null}

      {actionError !== undefined ? (
        <Alert
          severity="error"
          onClose={onDismissAction}
          data-testid="admin-identity-providers-action-error"
        >
          {actionError === "delete"
            ? t(
                "pages.admin.identityProviders.error.delete",
                "Failed to remove that identity provider.",
              )
            : actionError}
        </Alert>
      ) : null}
    </>
  );
}

/** The editor under the read-only wrapper of its own item (R3.4b, issue 2613). */
export function AdminIdentityProvidersEditor() {
  return (
    <AdminWritable itemId={ADMIN_ITEM.identityProviders}>
      <IdentityProvidersEditor />
    </AdminWritable>
  );
}

function IdentityProvidersEditor() {
  const access = useAdminWritable(ADMIN_ITEM.identityProviders);
  const trustLocked = useIdentityTrustLocked();
  const listQuery = useAdminIdentityProviders();
  const saveMutation = access.guard(useSaveAdminIdentityProvider());
  const deleteMutation = access.guard(useDeleteAdminIdentityProvider());

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<AdminIdentityProvider | undefined>(
    undefined,
  );
  const [pending, setPending] = useState<PendingProviderAction | undefined>(
    undefined,
  );
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const [actionError, setActionError] = useState<string | undefined>(undefined);
  const [rebindIssuer, setRebindIssuer] = useState<string | undefined>(
    undefined,
  );

  // `listQuery.data ?? []` would allocate a new array every render, so the memo
  // below would never hit. Memoising the fallback is what makes the dependency
  // stable.
  const providers = useMemo(() => listQuery.data ?? [], [listQuery.data]);
  const existingKeys = useMemo(
    () => new Set(providers.map((provider) => provider.key)),
    [providers],
  );

  const openCreate = (): void => {
    setEditing(undefined);
    setSaveError(undefined);
    setDialogOpen(true);
  };

  const openEdit = (provider: AdminIdentityProvider): void => {
    setEditing(provider);
    setSaveError(undefined);
    setDialogOpen(true);
  };

  const handleSubmit = (draft: AdminIdentityProviderDraft, reason: AdminReason): void => {
    setSaveError(undefined);
    saveMutation.mutate({ draft, reason }, {
      onSuccess: (result) => {
        setDialogOpen(false);
        setEditing(undefined);
        setRebindIssuer(result.previousIssuer);
      },
      // The dialog STAYS OPEN on failure, holding what was typed. Closing it
      // would discard the operator's input along with the reason it was
      // refused — and on this screen that input includes a secret they would
      // have to find again.
      onError: (error: unknown) => {
        setSaveError(identityTrustFailureReason(error) ?? "save");
      },
    });
  };

  const handleConfirm = (reason: AdminReason): void => {
    if (pending === undefined) return;
    if (pending.kind === "save") {
      setPending(undefined);
      handleSubmit(pending.draft, reason);
      return;
    }
    setActionError(undefined);
    deleteMutation.mutate({ key: pending.provider.key, reason }, {
      onSuccess: () => {
        setPending(undefined);
      },
      onError: (error: unknown) => {
        // Deleting a provider is a trust action (ADR-0059 R0.4c): an
        // `admin` gets the translated super admin refusal.
        setActionError(identityTrustFailureReason(error) ?? "delete");
        setPending(undefined);
      },
    });
  };

  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <AdminViewOnlyNotice />
      <AdminIdentityTrustNotice subject="providers" />
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          "pages.admin.identityProviders.description",
          "The identity providers this deployment federates logins through. Each secret is sealed in the platform vault and is never returned by any endpoint. One provider of each protocol can be live at a time.",
        )}
      </Typography>

      {/* Stated once, on the page, rather than discovered when a save appears to
          do nothing. Editing a live provider takes effect on the next login;
          introducing the first one on a deployment that federated none needs a
          restart, because which browser-auth plane owns /auth is fixed
          at boot. */}
      <Alert
        severity="info"
        data-testid="admin-identity-providers-restart-note"
      >
        {t(
          "pages.admin.identityProviders.restartNote",
          "Changes to a provider take effect on the next login. Adding the first provider to a deployment that used no single sign-on needs a service restart before its login routes are served.",
        )}
      </Alert>

      {listQuery.isLoading ? <LinearProgress /> : null}

      <ProviderAlerts
        loadError={listQuery.error}
        actionError={actionError}
        onDismissAction={() => {
          setActionError(undefined);
        }}
        rebindIssuer={rebindIssuer}
        onDismissRebind={() => {
          setRebindIssuer(undefined);
        }}
      />

      <Box>
        <Button
          size="small"
          variant="elitea" color="primary"
          onClick={openCreate}
          // Adding a provider is a trust action (ADR-0059 R0.4c).
          disabled={!access.writable || trustLocked}
          sx={{ textTransform: "none" }}
          data-testid="admin-identity-providers-add"
        >
          {t("pages.admin.identityProviders.add", "Add identity provider")}
        </Button>
      </Box>

      {!listQuery.isLoading &&
      providers.length === 0 &&
      listQuery.error == null ? (
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          data-testid="admin-identity-providers-empty"
        >
          {t(
            "pages.admin.identityProviders.empty",
            "No identity provider is authored. This deployment federates logins only if its environment configures one.",
          )}
        </Typography>
      ) : null}

      {providers.length > 0 ? (
        <ProviderTable
          providers={providers}
          onEdit={openEdit}
          onRemove={(provider) => {
            setPending({ kind: "delete", provider });
          }}
        />
      ) : null}

      <AdminIdentityProviderDialog
        open={dialogOpen}
        editing={editing}
        existingKeys={existingKeys}
        isSaving={saveMutation.isPending}
        trustLocked={trustLocked}
        serverError={
          saveError === "save"
            ? t(
                "pages.admin.identityProviders.error.save",
                "Failed to save that identity provider.",
              )
            : saveError
        }
        onClose={() => {
          setDialogOpen(false);
          setEditing(undefined);
        }}
        onSubmit={(draft) => {
          setPending({ kind: "save", draft });
        }}
      />

      {pending !== undefined ? (
        <AdminDangerousActionDialog
          open
          {...providerDialog(pending)}
          pending={confirmBlocked(pending, {
            saving: saveMutation.isPending || deleteMutation.isPending,
            locked: !access.writable || trustLocked,
          })}
          onClose={() => {
            setPending(undefined);
          }}
          onConfirm={handleConfirm}
        />
      ) : null}

    </Box>
  );
}
