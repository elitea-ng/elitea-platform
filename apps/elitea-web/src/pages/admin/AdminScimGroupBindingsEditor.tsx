/**
 * Admin › Configuration › Authentication — the SCIM group bindings.
 *
 * It renders under the identity providers, because it is the other half of the
 * same story: a provider federates the LOGIN, and SCIM pushes the DIRECTORY.
 *
 * ## What a binding is for
 *
 * A SCIM group carries a name and a list of members. Membership here is always
 * (user, project, role), so a group says nothing about half of what it would
 * have to grant. A binding is that half, authored here: one group, one project,
 * one role. A group the identity provider pushes with no binding is refused and
 * named — it never creates a project, and it never guesses a role.
 *
 * ## What the table says, and why the `granted` column exists
 *
 * A member the group GRANTED loses the role when they leave the group. A member
 * who already held it keeps it: the identity provider is authoritative over
 * what it gave, not over what it found. Without that distinction on screen, an
 * operator would remove somebody from a group and expect them out of the
 * project.
 */
import { useMemo, useState } from "react";

import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import LinearProgress from "@mui/material/LinearProgress";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

import { AdminScimGroupBindingDialog } from "./AdminScimGroupBindingDialog";
import { AdminScimGroupBindingTable } from "./AdminScimGroupBindingTable";
import { configFailureReason } from "./api/adminConfigurationApi";
import {
  useAdminScimGroupBindings,
  useDeleteAdminScimGroupBinding,
  useSaveAdminScimGroupBinding,
  type AdminScimGroupBinding,
  type AdminScimGroupBindingDraft,
} from "./api/adminScimGroupBindingsApi";

export function AdminScimGroupBindingsEditor() {
  // The page offset this screen is showing. A binding past the first page must
  // stay reachable: an operator who cannot find one authors a duplicate, and
  // the unique group name then refuses it for a reason no screen explains.
  const [offset, setOffset] = useState(0);
  const listQuery = useAdminScimGroupBindings(offset);
  const saveMutation = useSaveAdminScimGroupBinding();
  const deleteMutation = useDeleteAdminScimGroupBinding();

  const [dialogOpen, setDialogOpen] = useState(false);
  const [editing, setEditing] = useState<AdminScimGroupBinding | undefined>(
    undefined,
  );
  const [pendingDelete, setPendingDelete] = useState<
    AdminScimGroupBinding | undefined
  >(undefined);
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const [actionError, setActionError] = useState<string | undefined>(undefined);

  const listQueryPage = listQuery.data;
  // A new array each render would defeat every memo downstream.
  const bindings = useMemo(
    () => listQueryPage?.bindings ?? [],
    [listQueryPage],
  );
  const total = listQueryPage?.total ?? bindings.length;
  const hasMore = offset + bindings.length < total;

  const handleSubmit = (draft: AdminScimGroupBindingDraft): void => {
    setSaveError(undefined);
    saveMutation.mutate(draft, {
      onSuccess: () => {
        setDialogOpen(false);
        setEditing(undefined);
      },
      // The dialog STAYS OPEN on a refusal, holding what was typed together
      // with the server's own sentence — which names the project or the role
      // that was wrong.
      onError: (error: unknown) => {
        setSaveError(
          configFailureReason(error) ??
            t(
              "pages.admin.scimGroups.error.save",
              "Failed to save that group binding.",
            ),
        );
      },
    });
  };

  const handleDelete = (): void => {
    if (pendingDelete === undefined) return;
    setActionError(undefined);
    deleteMutation.mutate(pendingDelete.id, {
      onSuccess: () => {
        setPendingDelete(undefined);
      },
      onError: (error: unknown) => {
        setActionError(
          configFailureReason(error) ??
            t(
              "pages.admin.scimGroups.error.delete",
              "Failed to remove that group binding.",
            ),
        );
        setPendingDelete(undefined);
      },
    });
  };

  return (
    <Box
      sx={{ display: "flex", flexDirection: "column", gap: "1rem" }}
      data-testid="admin-scim-group-bindings"
    >
      <Typography variant="headingSmall" component="h2">
        {t(
          "pages.admin.scimGroups.title",
          "Directory group provisioning (SCIM)",
        )}
      </Typography>

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          "pages.admin.scimGroups.description",
          "A directory group grants one role on one project. Bind the group here first: a group an identity provider pushes with no binding is refused, and a push never creates or deletes a project.",
        )}
      </Typography>

      {listQuery.isLoading ? <LinearProgress /> : null}

      {listQuery.error != null ? (
        <Alert severity="warning" data-testid="admin-scim-group-bindings-error">
          {configFailureReason(listQuery.error) ??
            t(
              "pages.admin.scimGroups.error.load",
              "Failed to load the group bindings.",
            )}
        </Alert>
      ) : null}

      {actionError !== undefined ? (
        <Alert
          severity="error"
          onClose={() => {
            setActionError(undefined);
          }}
          data-testid="admin-scim-group-bindings-action-error"
        >
          {actionError}
        </Alert>
      ) : null}

      <Box>
        <Button
          size="small"
          variant="elitea" color="primary"
          onClick={() => {
            setEditing(undefined);
            setSaveError(undefined);
            setDialogOpen(true);
          }}
          sx={{ textTransform: "none" }}
          data-testid="admin-scim-group-bindings-add"
        >
          {t("pages.admin.scimGroups.add", "Bind a group")}
        </Button>
      </Box>

      {!listQuery.isLoading &&
      bindings.length === 0 &&
      listQuery.error == null ? (
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          data-testid="admin-scim-group-bindings-empty"
        >
          {t(
            "pages.admin.scimGroups.empty",
            "No directory group is bound. Group provisioning refuses every push until a group is bound to a project and a role.",
          )}
        </Typography>
      ) : null}

      <AdminScimGroupBindingTable
        bindings={bindings}
        total={total}
        offset={offset}
        hasMore={hasMore}
        busy={listQuery.isFetching}
        onEdit={(binding) => {
          setEditing(binding);
          setSaveError(undefined);
          setDialogOpen(true);
        }}
        onRemove={setPendingDelete}
        onOffset={setOffset}
      />

      <AdminScimGroupBindingDialog
        open={dialogOpen}
        editing={editing}
        isSaving={saveMutation.isPending}
        serverError={saveError}
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
        data-testid="admin-scim-binding-delete-dialog"
      >
        <DialogTitle>
          {t("pages.admin.scimGroups.delete.title", "Remove group binding")}
        </DialogTitle>
        <DialogContent>
          <Typography variant="bodyMedium">
            {t(
              "pages.admin.scimGroups.delete.body",
              "This withdraws the role “{{name}}” gave its members and removes the binding. The project, its content and any member added by hand are not affected, and the identity provider is refused until the group is bound again.",
              { name: pendingDelete?.display_name ?? "" },
            )}
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
            {t("pages.admin.scimGroups.delete.cancel", "Cancel")}
          </Button>
          <Button
            variant="elitea" color="alarm"
            onClick={handleDelete}
            disabled={deleteMutation.isPending}
            sx={{ textTransform: "none" }}
            data-testid="admin-scim-binding-delete-confirm"
          >
            {t("pages.admin.scimGroups.delete.confirm", "Remove")}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
