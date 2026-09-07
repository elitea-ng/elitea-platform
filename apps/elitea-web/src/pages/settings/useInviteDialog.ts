/**
 * The invite dialog's own state: whether it is open, and the server's
 * per-address answer to the last submit.
 *
 * It is a hook rather than four `useState` lines on the page because the two
 * pieces move TOGETHER and always did — every transition that closes the dialog
 * must also drop the rows, and every new submit must drop the previous
 * submit's rows before the request goes out. Keeping the pair in one place is
 * what stops the previous batch's failures from being rendered under the next
 * batch's addresses.
 */
import { useCallback, useEffect, useState } from 'react';

import { useNavigate, useSearch } from '@tanstack/react-router';

import type { InviteAddressResult } from '@/shared/ui/settings/inviteResults';

export interface InviteDialogState {
  readonly open: boolean;
  readonly results: readonly InviteAddressResult[];
  /** Open or close the dialog. Closing always clears the rows. */
  readonly setOpen: (next: boolean) => void;
  /** Close and clear — what a fully successful batch does. */
  readonly close: () => void;
  /** Publish the rows a partial batch came back with, keeping the dialog open. */
  readonly showResults: (rows: readonly InviteAddressResult[]) => void;
  /** Drop the rows without closing — what a new submit does before it fires. */
  readonly clearResults: () => void;
}

export function useInviteDialog(): InviteDialogState {
  const [open, setOpenState] = useState(false);
  const [results, setResults] = useState<readonly InviteAddressResult[]>([]);

  const setOpen = useCallback((next: boolean) => {
    setOpenState(next);
    if (!next) setResults([]);
  }, []);

  const close = useCallback(() => {
    setOpenState(false);
    setResults([]);
  }, []);

  const showResults = useCallback((rows: readonly InviteAddressResult[]) => setResults(rows), []);
  const clearResults = useCallback(() => setResults([]), []);

  // `?inviteUsers=1` deep link (reference parity: `Users.jsx`'s `shouldInvite`
  // effect) — open the dialog once, then strip the flag from the URL so a
  // reload or a Back does not re-open it.
  const navigate = useNavigate();
  const routeSearch = useSearch({ strict: false }) as { inviteUsers?: string };
  useEffect(() => {
    if (routeSearch.inviteUsers !== '1') return;
    setOpenState(true);
    void navigate({ to: '/settings/users', search: {}, replace: true });
  }, [routeSearch.inviteUsers, navigate]);

  return { open, results, setOpen, close, showResults, clearResults };
}
