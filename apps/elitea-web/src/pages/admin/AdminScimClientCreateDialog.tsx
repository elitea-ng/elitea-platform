/**
 * The "New SCIM client" dialog: a name and an authentication method.
 *
 * It STAYS OPEN on a refusal, holding what was typed together with the
 * server's own sentence (a duplicate name, a name too long).
 */
import { useEffect, useState } from "react";

import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import FormControl from "@mui/material/FormControl";
import FormControlLabel from "@mui/material/FormControlLabel";
import FormLabel from "@mui/material/FormLabel";
import Radio from "@mui/material/Radio";
import RadioGroup from "@mui/material/RadioGroup";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

import {
  SCIM_MAX_LIFETIME_DAYS,
  dateInputValue,
  expiryInstant,
} from "./AdminScimClientExpiry";
import { scimMethodLabel } from "./AdminScimClientTable";
import type {
  AdminScimClientDraft,
  ScimClientAuthMethod,
} from "./api/adminScimClientsApi";

const NAME_MAX = 100;

const AUTH_METHODS: readonly ScimClientAuthMethod[] = [
  "bearer",
  "client_credentials",
];

function methodHint(method: ScimClientAuthMethod): string {
  return method === "client_credentials"
    ? t(
        "pages.admin.scimClients.method.clientCredentialsHint",
        "The identity provider exchanges a client ID and secret at the token endpoint for short-lived access tokens.",
      )
    : t(
        "pages.admin.scimClients.method.bearerHint",
        "The identity provider sends one long-lived secret token with every request.",
      );
}

export interface AdminScimClientCreateDialogProps {
  readonly open: boolean;
  readonly isSaving: boolean;
  readonly serverError: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (draft: AdminScimClientDraft) => void;
}

export function AdminScimClientCreateDialog({
  open,
  isSaving,
  serverError,
  onClose,
  onSubmit,
}: AdminScimClientCreateDialogProps) {
  const [name, setName] = useState("");
  const [authMethod, setAuthMethod] = useState<ScimClientAuthMethod>("bearer");
  // `yyyy-mm-dd` or empty for "no expiry".
  const [expiresOn, setExpiresOn] = useState("");

  // A fresh dialog each time it opens; a refusal keeps what was typed because
  // the dialog does not close on one.
  useEffect(() => {
    if (open) {
      setName("");
      setAuthMethod("bearer");
      setExpiresOn("");
    }
  }, [open]);

  const trimmed = name.trim();
  // The date input bounds: tomorrow to two years ahead, the server's range.
  const today = new Date();
  const minDate = dateInputValue(
    new Date(today.getFullYear(), today.getMonth(), today.getDate() + 1),
  );
  const maxDate = dateInputValue(
    new Date(
      today.getFullYear(),
      today.getMonth(),
      today.getDate() + SCIM_MAX_LIFETIME_DAYS - 1,
    ),
  );
  const expiryValid =
    expiresOn === "" ||
    (expiryInstant(expiresOn) !== undefined &&
      expiresOn >= minDate &&
      expiresOn <= maxDate);
  const valid = trimmed.length > 0 && trimmed.length <= NAME_MAX && expiryValid;

  return (
    <Dialog
      open={open}
      onClose={onClose}
      maxWidth="sm"
      fullWidth
      aria-labelledby="admin-scim-client-create-title"
      data-testid="admin-scim-client-create-dialog"
    >
      <DialogTitle id="admin-scim-client-create-title">
        {t("pages.admin.scimClients.create.title", "New SCIM client")}
      </DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {serverError !== undefined ? (
            <Alert
              severity="error"
              data-testid="admin-scim-client-create-error"
            >
              {serverError}
            </Alert>
          ) : null}
          <TextField
            label={t("pages.admin.scimClients.create.name", "Name")}
            value={name}
            onChange={(event) => {
              setName(event.target.value);
            }}
            required
            fullWidth
            size="small"
            helperText={t(
              "pages.admin.scimClients.create.nameHint",
              "For example the identity provider and tenant, such as “Entra ID production”.",
            )}
            slotProps={{
              htmlInput: {
                maxLength: NAME_MAX,
                "data-testid": "admin-scim-client-name",
              },
            }}
          />
          <TextField
            label={t(
              "pages.admin.scimClients.create.expiresOn",
              "Expires on (optional)",
            )}
            type="date"
            value={expiresOn}
            onChange={(event) => {
              setExpiresOn(event.target.value);
            }}
            fullWidth
            size="small"
            error={!expiryValid}
            helperText={
              expiryValid
                ? t(
                    "pages.admin.scimClients.create.expiresOnHint",
                    "Leave empty for no expiry. After this date the client stops working; the list warns 30 days before.",
                  )
                : t(
                    "pages.admin.scimClients.create.expiresOnInvalid",
                    "Choose a date from tomorrow to two years ahead.",
                  )
            }
            slotProps={{
              inputLabel: { shrink: true },
              htmlInput: {
                min: minDate,
                max: maxDate,
                "data-testid": "admin-scim-client-expires-on",
              },
            }}
          />
          <FormControl>
            <FormLabel id="admin-scim-client-method-label">
              {t(
                "pages.admin.scimClients.create.method",
                "Authentication method",
              )}
            </FormLabel>
            <RadioGroup
              aria-labelledby="admin-scim-client-method-label"
              value={authMethod}
              onChange={(event) => {
                setAuthMethod(event.target.value as ScimClientAuthMethod);
              }}
            >
              {AUTH_METHODS.map((method) => (
                <FormControlLabel
                  key={method}
                  value={method}
                  control={<Radio />}
                  data-testid={`admin-scim-client-method-${method}`}
                  label={
                    <Stack>
                      <Typography variant="bodyMedium">
                        {scimMethodLabel(method)}
                      </Typography>
                      <Typography variant="bodySmall" color="text.secondary">
                        {methodHint(method)}
                      </Typography>
                    </Stack>
                  }
                />
              ))}
            </RadioGroup>
          </FormControl>
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button
          onClick={onClose}
          disabled={isSaving}
          sx={{ textTransform: "none" }}
        >
          {t("pages.admin.scimClients.cancel", "Cancel")}
        </Button>
        <Button
          variant="elitea"
          color="primary"
          disabled={!valid || isSaving}
          onClick={() => {
            onSubmit({
              name: trimmed,
              authMethod,
              expiresAt: expiresOn === "" ? undefined : expiryInstant(expiresOn),
            });
          }}
          sx={{ textTransform: "none" }}
          data-testid="admin-scim-client-create-submit"
        >
          {t("pages.admin.scimClients.create.submit", "Create")}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
