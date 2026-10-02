/**
 * Admin › Configuration › Authentication — the SCIM client table.
 *
 * A row never carries a secret. It shows the last four characters so an
 * operator can match a row to the value configured in the identity provider,
 * and nothing that would let the secret be reconstructed.
 *
 * Rotate and Revoke are offered on an ACTIVE client only: a revoked client is
 * kept as a record of what existed, and Delete removes it.
 */
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

import type {
  AdminScimClient,
  ScimClientAuthMethod,
} from "./api/adminScimClientsApi";

/** The method's operator-facing name, shared with the create dialog. */
export function scimMethodLabel(method: ScimClientAuthMethod): string {
  return method === "client_credentials"
    ? t("pages.admin.scimClients.method.clientCredentials", "OAuth2 client credentials")
    : t("pages.admin.scimClients.method.bearer", "Secret token (Bearer)");
}

function formatDate(value: string | null | undefined): string | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

function CreatedCell({ client }: { readonly client: AdminScimClient }) {
  const when = formatDate(client.created_at) ?? "";
  const by = client.created_by_name;
  return (
    <Typography variant="bodySmall">
      {by === undefined || by === ""
        ? when
        : t("pages.admin.scimClients.table.createdBy", "{{date}} by {{name}}", {
            date: when,
            name: by,
          })}
    </Typography>
  );
}

export type AdminScimClientAction = "rotate" | "revoke" | "delete";

export interface AdminScimClientTableProps {
  readonly clients: readonly AdminScimClient[];
  readonly busy: boolean;
  readonly onAction: (
    action: AdminScimClientAction,
    client: AdminScimClient,
  ) => void;
}

function ClientRow({
  client,
  busy,
  onAction,
}: {
  readonly client: AdminScimClient;
  readonly busy: boolean;
  readonly onAction: AdminScimClientTableProps["onAction"];
}) {
  const active = client.status === "active";
  return (
    <TableRow data-testid={`admin-scim-client-row-${client.id}`}>
      <TableCell>{client.name}</TableCell>
      <TableCell>{scimMethodLabel(client.auth_method)}</TableCell>
      <TableCell>
        {client.auth_method === "client_credentials" &&
        client.client_id !== undefined ? (
          <Typography variant="bodySmall" sx={{ fontFamily: "monospace" }}>
            {client.client_id}
          </Typography>
        ) : null}
      </TableCell>
      <TableCell>
        <Typography variant="bodySmall" sx={{ fontFamily: "monospace" }}>
          {`…${client.secret_hint}`}
        </Typography>
      </TableCell>
      <TableCell>
        <CreatedCell client={client} />
      </TableCell>
      <TableCell>
        <Typography variant="bodySmall">
          {formatDate(client.last_used_at) ??
            t("pages.admin.scimClients.table.never", "Never")}
        </Typography>
      </TableCell>
      <TableCell>
        <Chip
          size="small"
          color={active ? "success" : "default"}
          label={
            active
              ? t("pages.admin.scimClients.status.active", "Active")
              : client.status === "expired"
                ? t("pages.admin.scimClients.status.expired", "Expired")
                : t("pages.admin.scimClients.status.revoked", "Revoked")
          }
        />
      </TableCell>
      <TableCell align="right">
        <Stack direction="row" spacing={1} sx={{ justifyContent: "flex-end" }}>
          {active ? (
            <Button
              size="small"
              disabled={busy}
              onClick={() => {
                onAction("rotate", client);
              }}
              sx={{ textTransform: "none" }}
              data-testid={`admin-scim-client-rotate-${client.id}`}
            >
              {t("pages.admin.scimClients.action.rotate", "Rotate")}
            </Button>
          ) : null}
          {active ? (
            <Button
              size="small"
              disabled={busy}
              onClick={() => {
                onAction("revoke", client);
              }}
              sx={{ textTransform: "none" }}
              data-testid={`admin-scim-client-revoke-${client.id}`}
            >
              {t("pages.admin.scimClients.action.revoke", "Revoke")}
            </Button>
          ) : null}
          <Button
            size="small"
            color="error"
            disabled={busy}
            onClick={() => {
              onAction("delete", client);
            }}
            sx={{ textTransform: "none" }}
            data-testid={`admin-scim-client-delete-${client.id}`}
          >
            {t("pages.admin.scimClients.action.delete", "Delete")}
          </Button>
        </Stack>
      </TableCell>
    </TableRow>
  );
}

export function AdminScimClientTable({
  clients,
  busy,
  onAction,
}: AdminScimClientTableProps) {
  if (clients.length === 0) return null;
  return (
    <Table
      size="small"
      aria-label={t("pages.admin.scimClients.table.label", "SCIM clients")}
      data-testid="admin-scim-clients-table"
    >
      <TableHead>
        <TableRow>
          <TableCell>{t("pages.admin.scimClients.table.name", "Name")}</TableCell>
          <TableCell>
            {t("pages.admin.scimClients.table.method", "Method")}
          </TableCell>
          <TableCell>
            {t("pages.admin.scimClients.table.clientId", "Client ID")}
          </TableCell>
          <TableCell>
            {t("pages.admin.scimClients.table.secret", "Secret ending")}
          </TableCell>
          <TableCell>
            {t("pages.admin.scimClients.table.created", "Created")}
          </TableCell>
          <TableCell>
            {t("pages.admin.scimClients.table.lastUsed", "Last used")}
          </TableCell>
          <TableCell>
            {t("pages.admin.scimClients.table.status", "Status")}
          </TableCell>
          <TableCell align="right">
            {t("pages.admin.scimClients.table.actions", "Actions")}
          </TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {clients.map((client) => (
          <ClientRow
            key={client.id}
            client={client}
            busy={busy}
            onAction={onAction}
          />
        ))}
      </TableBody>
    </Table>
  );
}
