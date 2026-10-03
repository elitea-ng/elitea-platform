/**
 * A read-only, labelled value with a copy button — the Tenant URL, the token
 * endpoint and the one-time secrets on the SCIM provisioning panel.
 *
 * Not `shared/ui/CopyToClipboardButton`: that renders the VALUE as the button's
 * text, which is the wrong shape for a field an operator has to read, select
 * and paste into an identity provider's form. The clipboard write itself goes
 * through `shared/lib/clipboard`'s `handleCopy`, so the fallback behaviour is
 * shared with every other copy site.
 */
import { useEffect, useState } from "react";

import ContentCopyIcon from "@mui/icons-material/ContentCopy";
import IconButton from "@mui/material/IconButton";
import InputAdornment from "@mui/material/InputAdornment";
import TextField from "@mui/material/TextField";
import Tooltip from "@mui/material/Tooltip";

import { t } from "@/shared/i18n";
import { handleCopy } from "@/shared/lib/clipboard";

export interface AdminScimCopyFieldProps {
  readonly label: string;
  readonly value: string;
  readonly testId: string;
}

export function AdminScimCopyField({
  label,
  value,
  testId,
}: AdminScimCopyFieldProps) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return undefined;
    const timer = window.setTimeout(() => {
      setCopied(false);
    }, 2000);
    return () => {
      window.clearTimeout(timer);
    };
  }, [copied]);

  const copyLabel = copied
    ? t("pages.admin.scimClients.copied", "Copied")
    : t("pages.admin.scimClients.copy", "Copy {{label}}", { label });

  return (
    <TextField
      label={label}
      value={value}
      size="small"
      fullWidth
      slotProps={{
        input: {
          readOnly: true,
          endAdornment: (
            <InputAdornment position="end">
              <Tooltip title={copyLabel}>
                <IconButton
                  size="small"
                  aria-label={copyLabel}
                  onClick={() => {
                    void handleCopy(value).then(() => {
                      setCopied(true);
                    });
                  }}
                  data-testid={`${testId}-copy`}
                >
                  <ContentCopyIcon fontSize="small" />
                </IconButton>
              </Tooltip>
            </InputAdornment>
          ),
        },
        htmlInput: { "data-testid": testId },
      }}
    />
  );
}
