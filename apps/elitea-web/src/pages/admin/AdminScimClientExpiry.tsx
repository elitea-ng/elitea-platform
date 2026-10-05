/**
 * The expiry of a SCIM client: an optional end date set at creation.
 *
 * After it the client stops authenticating, exactly as if it were revoked, and
 * provisioning stops. The table therefore says how many days are left, and
 * marks the last 30 so an operator rotates or replaces the client in time.
 */
import Chip from "@mui/material/Chip";
import Typography from "@mui/material/Typography";

import { t } from "@/shared/i18n";

/** Days before the expiry at which the table starts to warn. */
export const SCIM_EXPIRY_WARNING_DAYS = 30;
/** The furthest expiry the server accepts (two years). */
export const SCIM_MAX_LIFETIME_DAYS = 730;

const DAY_MS = 24 * 60 * 60 * 1000;

/** Whole days from `now` to `expiresAt`, rounded up; negative once past. */
export function daysUntil(expiresAt: string, now: Date = new Date()): number {
  return Math.ceil((new Date(expiresAt).getTime() - now.getTime()) / DAY_MS);
}

/** `yyyy-mm-dd` for a date input, in local time. */
export function dateInputValue(date: Date): string {
  const month = String(date.getMonth() + 1).padStart(2, "0");
  const day = String(date.getDate()).padStart(2, "0");
  return `${date.getFullYear()}-${month}-${day}`;
}

/**
 * The RFC 3339 instant a `yyyy-mm-dd` expiry means: the END of that local day,
 * so a client set to expire "on" a date still works for the whole of it.
 */
export function expiryInstant(dateValue: string): string | undefined {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(dateValue);
  if (match === null) return undefined;
  const end = new Date(
    Number(match[1]),
    Number(match[2]) - 1,
    Number(match[3]),
    23,
    59,
    59,
  );
  return Number.isNaN(end.getTime()) ? undefined : end.toISOString();
}

export function ScimClientExpiryCell({
  expiresAt,
}: {
  readonly expiresAt: string | null | undefined;
}) {
  if (expiresAt === undefined || expiresAt === null || expiresAt === "") {
    return (
      <Typography variant="bodySmall" color="text.secondary">
        {t("pages.admin.scimClients.expiry.none", "No expiry")}
      </Typography>
    );
  }
  const days = daysUntil(expiresAt);
  if (days <= 0) {
    return (
      <Typography variant="bodySmall">
        {t("pages.admin.scimClients.expiry.expired", "Expired {{date}}", {
          date: new Date(expiresAt).toLocaleDateString(),
        })}
      </Typography>
    );
  }
  const label = t(
    "pages.admin.scimClients.expiry.inDays",
    "Expires in {{count}} days",
    { count: days },
  );
  return days <= SCIM_EXPIRY_WARNING_DAYS ? (
    <Chip
      size="small"
      color="warning"
      label={label}
      data-testid="admin-scim-client-expiry-warning"
    />
  ) : (
    <Typography variant="bodySmall">{label}</Typography>
  );
}
