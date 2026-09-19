/**
 * The toolkit form's own editable (non-disabled) secret field — ported from
 * `ToolBaseProperty.jsx:324-339`, split out of `ToolBaseProperty.renderers.tsx`
 * to keep that file inside the §3.5 400-line budget.
 *
 * `secrets` comes from `useSecretFieldOptions()` (#441 — see that hook's
 * header): no caller supplied it before, so the mode toggle, the saved-secret
 * picker and its "Create new secret" entry were off for every user, an
 * administrator included. The hook queries, so it runs in this leaf, which
 * mounts for a secret field only.
 */
import type { ReactNode } from 'react';

import { useSecretFieldOptions } from '@/entities/secret';
import { SecretManagementInput } from '@/shared/ui/SecretManagementInput';

export interface SecretFieldProps {
  readonly value: string | undefined;
  readonly onChange: (value: string) => void;
  readonly label: string;
  readonly required: boolean;
  readonly error: boolean;
  readonly helperText: string | undefined;
  /** #902: names the "Create new secret" entry after the project scope; `undefined` keeps the generic label. */
  readonly isTeamProject?: boolean | undefined;
}

/** The editable (non-disabled) secret field — ported from `ToolBaseProperty.jsx:324-339`. `secrets` comes from `useSecretFieldOptions()` (#441 — see that hook's header): no caller supplied it before, so the mode toggle, the saved-secret picker and its "Create new secret" entry were off for every user, an administrator included. The hook queries, so it runs in this leaf, which mounts for a secret field only. */
export function SecretFieldInput({ value, onChange, label, required, error, helperText, isTeamProject }: SecretFieldProps): ReactNode {
  const secrets = useSecretFieldOptions(isTeamProject === undefined ? {} : { isTeamProject });
  return (
    <SecretManagementInput
      value={value ?? ''}
      onChange={onChange}
      label={label}
      required={required}
      error={error}
      secrets={secrets}
      {...(helperText !== undefined ? { helperText } : {})}
    />
  );
}
