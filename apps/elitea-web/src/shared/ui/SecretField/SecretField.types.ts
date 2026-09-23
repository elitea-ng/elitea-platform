/**
 * ui/SecretField.types.ts — the shapes `SecretField.tsx` and
 * `SecretSelect.tsx` both need. Split into their own file so neither file
 * has to import the other for a type: `SecretField.tsx` imports the VALUE
 * `SecretSelect`/`CREATE_SECRET_VALUE` from `SecretSelect.tsx`, and
 * `SecretSelect.tsx` needed `SecretFieldSecretsOptions` back from
 * `SecretField.tsx` — exactly the shape `check-layer-cycle`'s `no-circular`
 * rule flags. Re-exported from `SecretField.tsx` so existing importers of
 * these names (e.g. this folder's `index.ts`) do not need to change.
 */

/** @public */
export type SecretFieldMode = 'secret' | 'password';

/** @public One entry in {@link SecretFieldSecretsOptions.options}. */
export interface SecretOption {
  label: string;
  value: string;
}

/** @public Everything about the "pick an existing secret" mode — omit entirely to render a plain masked text field with no mode toggle. */
export interface SecretFieldSecretsOptions {
  /** The caller's already-fetched secret list (replaces the baseline's internal `useSecretsListQuery`). */
  options?: SecretOption[];
  /** Caller may create a new secret (e.g. navigate to secret settings). Omit to hide the affordance. */
  onCreate?: () => void;
  /** Permission to create a secret, computed by the caller — replaces the baseline's internal `useCheckPermission(PERMISSIONS.secrets.create)` call. */
  canCreate?: boolean;
  createLabel?: string;
  /** Refresh the option list (e.g. after creating one out-of-band). Omit to hide the refresh action — replaces the baseline's internal RTK-Query `refetch`. */
  onRefresh?: () => void;
  /** Locks the field to whichever mode `value` currently implies — hides the toggle. */
  disableToggle?: boolean;
  tabLabels?: { secret?: string; password?: string };
}
