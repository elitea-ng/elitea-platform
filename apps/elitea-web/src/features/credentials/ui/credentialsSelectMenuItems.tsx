/**
 * lib/credentialsSelectMenuItems.tsx — the two families of `<MenuItem>` row
 * `CredentialsSelect.tsx` renders (the CREATE actions and one SAVED row),
 * split into their own file purely to keep that file under the §3.5
 * 400-line budget. Plain functions that RETURN `<MenuItem>` elements, called
 * directly (never rendered as JSX tags themselves) — see each function's own
 * doc comment for why that distinction matters to MUI's `Select`.
 */
import type { ReactNode } from 'react';

import MenuItem from '@mui/material/MenuItem';

import { encodeCreateActionValue, encodeSavedCredentialValue } from '../lib/credentialSelectValue';

import { CredentialCreateLabel } from './CredentialCreateLabel';
import { CredentialOptionLabel } from './CredentialOptionLabel';
import type { CredentialOptionRow, CredentialsSelectHandlers, CredentialsSelectState, CredentialsSelectValue } from './CredentialsSelect.types';

/**
 * The two "create new …" rows — split out of `CredentialsSelect` to keep
 * that function's cyclomatic complexity within the §3.5 budget. Returns a
 * plain array, NOT a `<>...</>` Fragment: MUI's `Select` walks
 * `props.children` directly (`Children.map`/`cloneElement` per child) to
 * find `MenuItem`s, and a Fragment wrapping them is opaque to that walk —
 * confirmed empirically (wrapping in a Fragment silently broke both
 * `onChange` selection and the create-action click in this component's own
 * tests, even though the items still rendered visually).
 */
export function renderCreateMenuItems(type: string | undefined): ReactNode[] {
  return [
    <MenuItem
      key="create-private"
      value={encodeCreateActionValue(true)}
    >
      <CredentialCreateLabel
        isPrivate
        {...(type !== undefined ? { type } : {})}
      />
    </MenuItem>,
    <MenuItem
      key="create-project"
      value={encodeCreateActionValue(false)}
    >
      <CredentialCreateLabel
        isPrivate={false}
        {...(type !== undefined ? { type } : {})}
      />
    </MenuItem>,
  ];
}

export interface SavedRowMenuItemProps {
  readonly row: CredentialOptionRow;
  readonly value: CredentialsSelectValue | null;
  readonly status: ReturnType<CredentialsSelectState['getStatus']>;
  readonly message: string;
  readonly onSelect: CredentialsSelectHandlers['onSelect'];
  readonly onRevalidate: CredentialsSelectHandlers['onRevalidate'];
}

/**
 * One saved-credential row — split out of `CredentialsSelect` for the same
 * complexity-budget reason as `renderCreateMenuItems`. A plain function
 * that RETURNS a `<MenuItem>` (called directly inside `.map()`, never
 * rendered as `<SavedRowMenuItem />`) — NOT a React component: MUI's
 * `Select` walks `props.children` for literal `MenuItem` elements, and a
 * wrapping custom component is opaque to that walk (confirmed empirically,
 * same class of bug `renderCreateMenuItems`'s doc comment records for a
 * Fragment; a wrapping component breaks it identically since neither is a
 * `MenuItem` at the `Select`'s own children level). Owns the
 * click-to-deselect `onClick` (see its own inline comment: MUI's `Select`
 * suppresses `onChange` on a same-value reselect, so this restores the
 * baseline's explicit toggle for exactly that one case).
 */
export function buildSavedRowMenuItem({ row, value, status, message, onSelect, onRevalidate }: SavedRowMenuItemProps): ReactNode {
  const isCurrentlySelected = value?.eliteaTitle === row.eliteaTitle && value.isPrivate === row.isPrivate;
  return (
    <MenuItem
      key={`${row.eliteaTitle}-${String(row.isPrivate)}`}
      value={encodeSavedCredentialValue({ eliteaTitle: row.eliteaTitle, isPrivate: row.isPrivate })}
      onClick={
        isCurrentlySelected
          ? () => {
              onSelect(null, { isAutoSelect: false });
            }
          : undefined
      }
    >
      <CredentialOptionLabel
        isPersonal={row.isPrivate}
        label={row.displayLabel}
        {...(row.credentialUrl !== undefined ? { credentialUrl: row.credentialUrl } : {})}
        isInvalid={status === 'invalid'}
        isChecking={status === 'checking'}
        invalidMessage={message}
        onRevalidate={(event) => {
          event.stopPropagation();
          onRevalidate(row.eliteaTitle);
        }}
      />
    </MenuItem>
  );
}
