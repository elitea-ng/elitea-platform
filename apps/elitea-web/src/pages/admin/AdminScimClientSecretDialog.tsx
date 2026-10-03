/**
 * The one-time reveal after a SCIM client is created or rotated.
 *
 * The server answers the plaintext secret once and keeps only a hash. This
 * dialog is the only place it is ever shown: the value arrives as a prop from
 * the editor's component state (never the query cache), and the editor drops
 * it when this dialog closes.
 */
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import Stack from "@mui/material/Stack";

import { t } from "@/shared/i18n";

import { AdminScimCopyField } from "./AdminScimCopyField";
import type { AdminScimClientSecret } from "./api/adminScimClientsApi";

export interface AdminScimClientSecretDialogProps {
  /** Undefined closes the dialog. */
  readonly reveal: AdminScimClientSecret | undefined;
  readonly tenantUrl: string;
  readonly tokenEndpoint: string;
  readonly onClose: () => void;
}

export function AdminScimClientSecretDialog({
  reveal,
  tenantUrl,
  tokenEndpoint,
  onClose,
}: AdminScimClientSecretDialogProps) {
  const isClientCredentials =
    reveal?.client.auth_method === "client_credentials";
  return (
    <Dialog
      open={reveal !== undefined}
      // Neither a backdrop click nor Escape may throw the only copy of the
      // secret away: only the explicit close button closes this dialog. The
      // dialog keeps its focus trap.
      onClose={() => undefined}
      maxWidth="sm"
      fullWidth
      aria-labelledby="admin-scim-client-secret-title"
      data-testid="admin-scim-client-secret-dialog"
    >
      <DialogTitle id="admin-scim-client-secret-title">
        {t(
          "pages.admin.scimClients.reveal.title",
          "SCIM client credentials for “{{name}}”",
          { name: reveal?.client.name ?? "" },
        )}
      </DialogTitle>
      <DialogContent>
        {reveal !== undefined ? (
          <Stack spacing={2} sx={{ pt: 1 }}>
            <Alert severity="warning">
              {t(
                "pages.admin.scimClients.reveal.warning",
                "Copy this secret now. It is shown only once and cannot be retrieved later.",
              )}
            </Alert>
            <AdminScimCopyField
              label={t("pages.admin.scimClients.tenantUrl", "Tenant URL")}
              value={tenantUrl}
              testId="admin-scim-client-reveal-tenant-url"
            />
            {isClientCredentials ? (
              <>
                <AdminScimCopyField
                  label={t(
                    "pages.admin.scimClients.reveal.clientId",
                    "Client identifier",
                  )}
                  value={reveal.clientId ?? ""}
                  testId="admin-scim-client-reveal-client-id"
                />
                <AdminScimCopyField
                  label={t(
                    "pages.admin.scimClients.reveal.clientSecret",
                    "Client secret",
                  )}
                  value={reveal.secret}
                  testId="admin-scim-client-reveal-secret"
                />
                <AdminScimCopyField
                  label={t(
                    "pages.admin.scimClients.tokenEndpoint",
                    "Token endpoint",
                  )}
                  value={tokenEndpoint}
                  testId="admin-scim-client-reveal-token-endpoint"
                />
              </>
            ) : (
              <AdminScimCopyField
                label={t(
                  "pages.admin.scimClients.reveal.secretToken",
                  "Secret token",
                )}
                value={reveal.secret}
                testId="admin-scim-client-reveal-secret"
              />
            )}
          </Stack>
        ) : null}
      </DialogContent>
      <DialogActions>
        <Button
          variant="elitea"
          color="primary"
          onClick={onClose}
          sx={{ textTransform: "none" }}
          data-testid="admin-scim-client-reveal-close"
        >
          {t("pages.admin.scimClients.reveal.close", "I have copied it")}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
