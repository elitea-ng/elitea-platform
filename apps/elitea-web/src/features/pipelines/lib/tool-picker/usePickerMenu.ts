import { useCallback, useState } from 'react';

import { useDebounce } from '@/shared/lib/useDebounce';

import { PICKER_SEARCH_DEBOUNCE } from './toolPicker.constants';

export interface UsePickerMenuResult {
  readonly anchorEl: HTMLElement | null;
  readonly isOpen: boolean;
  readonly hasOpened: boolean;
  readonly search: string;
  readonly setSearch: (value: string) => void;
  readonly query: string;
  readonly openMenu: (element: HTMLElement) => void;
  readonly closeMenu: () => void;
}

/** Open state and search of a picker menu. Lists load only once the menu has been opened. */
export function usePickerMenu(): UsePickerMenuResult {
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const [hasOpened, setHasOpened] = useState(false);
  const [search, setSearch] = useState('');
  const query = useDebounce(search, PICKER_SEARCH_DEBOUNCE);

  const openMenu = useCallback((element: HTMLElement) => {
    setAnchorEl(element);
    setHasOpened(true);
  }, []);

  const closeMenu = useCallback(() => {
    setAnchorEl(null);
    setSearch('');
  }, []);

  return {
    anchorEl,
    isOpen: Boolean(anchorEl),
    hasOpened,
    search,
    setSearch,
    query: query.trim(),
    openMenu,
    closeMenu,
  };
}
