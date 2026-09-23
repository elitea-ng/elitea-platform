/**
 * ui/CredentialsSelect.types.ts — the shape types `CredentialsSelect.tsx`
 * and `credentialsSelectMenuItems.tsx` both need. Split into their own
 * file so neither of those two files has to import the other just for a
 * type: a value-level import in each direction (`CredentialsSelect.tsx`
 * calls `buildSavedRowMenuItem`/`renderCreateMenuItems`,
 * `credentialsSelectMenuItems.tsx` needed these types back from
 * `CredentialsSelect.tsx`) is exactly the shape `check-layer-cycle`'s
 * `no-circular` rule flags — see that script's own doc comment. Re-exported
 * from `CredentialsSelect.tsx` so existing external imports of these names
 * (e.g. `CredentialsSelect.test.tsx`) do not need to change.
 */

export interface CredentialOptionRow {
  readonly eliteaTitle: string;
  readonly isPrivate: boolean;
  readonly displayLabel: string;
  readonly credentialUrl?: string;
  readonly shared?: boolean;
}

export interface CredentialsSelectValue {
  readonly eliteaTitle: string;
  readonly isPrivate: boolean;
}

export interface CredentialsSelectState {
  readonly configurations: readonly CredentialOptionRow[];
  readonly hasFetchedData: boolean;
  readonly isFetching: boolean;
  readonly getStatus: (eliteaTitle: string) => 'idle' | 'checking' | 'valid' | 'invalid' | 'unsupported';
  readonly getMessage: (eliteaTitle: string) => string;
}

export interface CredentialsSelectHandlers {
  readonly onSelect: (value: CredentialsSelectValue | null, meta: { isAutoSelect: boolean }) => void;
  readonly onRefresh: () => void;
  readonly onCreate: (isPrivate: boolean) => void;
  readonly onRevalidate: (eliteaTitle: string) => void;
}
