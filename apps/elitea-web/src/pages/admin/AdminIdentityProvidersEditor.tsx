/**
 * Admin › Configuration › Authentication — the identity provider editor.
 *
 * This is the section that used to render only a refusal about the Arbiter bus.
 * The refusal was accurate about the plugin-config VALUE endpoints, which still
 * cannot serve this section — two of its fields are credentials — and stopped
 * being the whole truth when federation acquired a store and a reader
 * (`./api/adminIdentityProvidersApi`, `elitea_auth.identity_providers`).
 *
 * The page reaches this component through a server-declared `managed_surface`,
 * never through a hardcoded section id — see `./Configuration.tsx`.
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
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import Divider from "@mui/material/Divider";
import LinearProgress from "@mui/material/LinearProgress";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

import { AdminIdentityProviderDialog } from "./AdminIdentityProviderDialog";
import { AdminScimClientsEditor } from "./AdminScimClientsEditor";
import { AdminScimGroupBindingsEditor } from "./AdminScimGroupBindingsEditor";
import { ProviderTable } from "./AdminIdentityProviderTable";
import { configFailureReason } from "./api/adminConfigurationApi";
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
}: {
  readonly loadError: unknown;
  readonly actionError: string | undefined;
  readonly onDismissAction: () => void;
}) {
  return (
    <>
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

export function AdminIdentityProvidersEditor() {
  const listQuery = useAdminIdentityProviders();
  const saveMutation = useSaveAdminIdentityProvider();
  const deleteMutation = useDeleteAdminIdentityProvider();

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<AdminIdentityProvider | undefined>(
    undefined,
  );
  const [pendingDelete, setPendingDelete] = useState<
    AdminIdentityProvider | undefined
  >(undefined);
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const [actionError, setActionError] = useState<string | undefined>(undefined);

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

  const handleSubmit = (draft: AdminIdentityProviderDraft): void => {
    setSaveError(undefined);
    saveMutation.mutate(draft, {
      onSuccess: () => {
        setDialogOpen(false);
        setEditing(undefined);
      },
      // The dialog STAYS OPEN on failure, holding what was typed. Closing it
      // would discard the operator's input along with the reason it was
      // refused — and on this screen that input includes a secret they would
      // have to find again.
      onError: (error: unknown) => {
        setSaveError(configFailureReason(error) ?? "save");
      },
    });
  };

  const handleDelete = (): void => {
    if (pendingDelete === undefined) return;
    setActionError(undefined);
    deleteMutation.mutate(pendingDelete.key, {
      onSuccess: () => {
        setPendingDelete(undefined);
      },
      onError: (error: unknown) => {
        setActionError(configFailureReason(error) ?? "delete");
        setPendingDelete(undefined);
      },
    });
  };

  return (
    <Box sx={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
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
      />

      <Box>
        <Button
          size="small"
          variant="elitea" color="primary"
          onClick={openCreate}
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
          onRemove={setPendingDelete}
        />
      ) : null}

      {/* SCIM group provisioning renders in the SAME section, under a divider,
          because it is the other half of one story: a provider federates the
          login, and SCIM pushes the directory. A section of its own would put
          two halves of the same configuration on two screens. */}
      <Divider sx={{ mt: "0.5rem" }} />
      {/* The SCIM credential comes first: an identity provider needs one
          before any group it pushes can be bound. */}
      <AdminScimClientsEditor />
      <Divider sx={{ mt: "0.5rem" }} />
      <AdminScimGroupBindingsEditor />

      <AdminIdentityProviderDialog
        open={dialogOpen}
        editing={editing}
        existingKeys={existingKeys}
        isSaving={saveMutation.isPending}
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
        onSubmit={handleSubmit}
      />

      <Dialog
        open={pendingDelete !== undefined}
        onClose={() => {
          setPendingDelete(undefined);
        }}
        maxWidth="xs"
        fullWidth
        data-testid="admin-identity-provider-delete-dialog"
      >
        <DialogTitle>
          {t(
            "pages.admin.identityProviders.delete.title",
            "Remove identity provider",
          )}
        </DialogTitle>
        <DialogContent>
          <Typography variant="bodyMedium">
            {deleteWarning(pendingDelete?.display_name ?? "")}
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button
            onClick={() => {
              setPendingDelete(undefined);
            }}
            disabled={deleteMutation.isPending}
            sx={{ textTransform: "none" }}
          >
            {t("pages.admin.identityProviders.delete.cancel", "Cancel")}
          </Button>
          <Button
            variant="elitea" color="alarm"
            onClick={handleDelete}
            disabled={deleteMutation.isPending}
            sx={{ textTransform: "none" }}
            data-testid="admin-identity-provider-delete-confirm"
          >
            {t("pages.admin.identityProviders.delete.confirm", "Remove")}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
