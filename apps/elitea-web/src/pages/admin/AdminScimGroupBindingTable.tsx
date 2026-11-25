/**
 * Admin › Configuration › Authentication — the group-binding table.
 *
 * ## Why `granted` is a column of its own
 *
 * A member the group GRANTED loses the role when they leave the group. A member
 * who already held it keeps it: the identity provider is authoritative over
 * what it gave, not over what it found. Without that distinction on screen an
 * operator removes somebody from a group and expects them out of the project.
 *
 * ## Why this pages
 *
 * A screen that renders its first page and says nothing about the rest is a
 * screen on which a binding past that page cannot be found, edited or removed.
 * An operator who cannot find one authors a duplicate, and the unique group
 * name refuses it for a reason no screen explains.
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

import { useAdminWriteControl } from "./adminWriteAccess";
import {
  SCIM_GROUP_BINDING_PAGE_SIZE,
  type AdminScimGroupBinding,
} from "./api/adminScimGroupBindingsApi";

/** The members cell. It names who the group granted and who it merely found. */
function MemberSummary({
  binding,
}: {
  readonly binding: AdminScimGroupBinding;
}) {
  const granted = binding.members.filter((member) => member.granted).length;
  const found = binding.members.length - granted;
  if (binding.members.length === 0) {
    return (
      <Typography variant="bodySmall" color="text.secondary">
        {t("pages.admin.scimGroups.table.noMembers", "No push has arrived yet")}
      </Typography>
    );
  }
  return (
    <Stack direction="row" spacing={1}>
      <Chip
        size="small"
        label={t("pages.admin.scimGroups.table.granted", "{{count}} granted", {
          count: granted,
        })}
      />
      {found > 0 ? (
        <Chip
          size="small"
          variant="outlined"
          label={t(
            "pages.admin.scimGroups.table.alreadyMembers",
            "{{count}} already members",
            {
              count: found,
            },
          )}
        />
      ) : null}
    </Stack>
  );
}

/** The project cell: the name when there is one, and always the id. */
function projectLabel(binding: AdminScimGroupBinding): string {
  if (binding.project_name === undefined || binding.project_name === "") {
    return `#${String(binding.project_id)}`;
  }
  return `${binding.project_name} (#${String(binding.project_id)})`;
}

export interface AdminScimGroupBindingTableProps {
  readonly bindings: readonly AdminScimGroupBinding[];
  readonly total: number;
  readonly offset: number;
  readonly hasMore: boolean;
  readonly busy: boolean;
  readonly onEdit: (binding: AdminScimGroupBinding) => void;
  readonly onRemove: (binding: AdminScimGroupBinding) => void;
  readonly onOffset: (offset: number) => void;
}

export function AdminScimGroupBindingTable({
  bindings,
  total,
  offset,
  hasMore,
  busy,
  onEdit,
  onRemove,
  onOffset,
}: AdminScimGroupBindingTableProps) {
  const viewOnly = useAdminWriteControl().disabled;
  if (bindings.length === 0) return null;
  return (
    <>
      <Table
        size="small"
        aria-label={t("pages.admin.scimGroups.table.label", "Group bindings")}
      >
        <TableHead>
          <TableRow>
            <TableCell>
              {t("pages.admin.scimGroups.table.group", "Group")}
            </TableCell>
            <TableCell>
              {t("pages.admin.scimGroups.table.project", "Project")}
            </TableCell>
            <TableCell>
              {t("pages.admin.scimGroups.table.role", "Role")}
            </TableCell>
            <TableCell>
              {t("pages.admin.scimGroups.table.members", "Members")}
            </TableCell>
            <TableCell align="right">
              {t("pages.admin.scimGroups.table.actions", "Actions")}
            </TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {bindings.map((binding) => (
            <TableRow
              key={binding.id}
              data-testid={`admin-scim-group-binding-${binding.id}`}
            >
              <TableCell>{binding.display_name}</TableCell>
              <TableCell>{projectLabel(binding)}</TableCell>
              <TableCell>{binding.role_name}</TableCell>
              <TableCell>
                <MemberSummary binding={binding} />
              </TableCell>
              <TableCell align="right">
                <Button
            variant="elitea" color="tertiary"
                  size="small"
                  disabled={viewOnly}
                  onClick={() => {
                    onEdit(binding);
                  }}
                  sx={{ textTransform: "none" }}
                >
                  {t("pages.admin.scimGroups.table.edit", "Edit")}
                </Button>
                <Button
            variant="elitea" color="alarm"
                  size="small"
                  disabled={viewOnly}
                  onClick={() => {
                    onRemove(binding);
                  }}
                  sx={{ textTransform: "none" }}
                >
                  {t("pages.admin.scimGroups.table.remove", "Remove")}
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

      {total > bindings.length || offset > 0 ? (
        <Stack
          direction="row"
          spacing={2}
          sx={{ alignItems: "center" }}
          data-testid="admin-scim-group-bindings-paging"
        >
          <Typography variant="bodySmall" color="text.secondary">
            {t(
              "pages.admin.scimGroups.paging.showing",
              "Showing {{from}}–{{to}} of {{total}} bindings",
              { from: offset + 1, to: offset + bindings.length, total },
            )}
          </Typography>
          <Button
            variant="elitea" color="tertiary"
            size="small"
            disabled={offset === 0 || busy}
            onClick={() => {
              onOffset(Math.max(0, offset - SCIM_GROUP_BINDING_PAGE_SIZE));
            }}
            sx={{ textTransform: "none" }}
            data-testid="admin-scim-group-bindings-previous"
          >
            {t("pages.admin.scimGroups.paging.previous", "Previous")}
          </Button>
          <Button
            variant="elitea" color="tertiary"
            size="small"
            disabled={!hasMore || busy}
            onClick={() => {
              onOffset(offset + SCIM_GROUP_BINDING_PAGE_SIZE);
            }}
            sx={{ textTransform: "none" }}
            data-testid="admin-scim-group-bindings-next"
          >
            {t("pages.admin.scimGroups.paging.next", "Next")}
          </Button>
        </Stack>
      ) : null}
    </>
  );
}
