/**
 * ui/CredentialsSearchField.tsx — the search/filter row `CredentialsSelect`
 * renders at the top of its popup (#919/ELITEA-2534). Split into its own
 * file purely to keep `CredentialsSelect.tsx` under the §3.5 400-line
 * budget.
 */
import type { ReactNode } from 'react';

import ListSubheader from '@mui/material/ListSubheader';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

/**
 * A `ListSubheader`, not a `MenuItem`: `Select` walks its children looking
 * for `MenuItem`s only (see `CredentialsSelect.tsx`'s `renderCreateMenuItems`
 * doc comment on that same walk), so this is inert to selection/value-matching
 * and exists purely to host the filter input. `stopPropagation` on both mouse
 * and keyboard events keeps typing (including Escape/arrow keys) from
 * reaching `Select`'s own handlers — without it, MUI's built-in "type to jump
 * to an option" behaviour steals keystrokes meant for this field.
 *
 * No `autoFocus`: `jsx-a11y/no-autofocus` (R-C1) bans the JSX prop outright
 * in this app — the same fence `shared/ui/SimpleSearchBar`'s own doc comment
 * (dropped there for the identical reason) already documents. The user
 * clicks or tabs into the field before typing, same as any other control.
 */
export function CredentialsSearchField(props: { readonly query: string; readonly onQueryChange: (next: string) => void }): ReactNode {
  const { query, onQueryChange } = props;
  return (
    <ListSubheader
      key="credentials-search"
      onClick={(event) => {
        event.stopPropagation();
      }}
      sx={searchListSubheaderSx}
    >
      <TextField
        size="small"
        fullWidth
        variant="outlined"
        placeholder={t('credentials.select.searchPlaceholder', 'Search credentials')}
        value={query}
        onChange={(event) => {
          onQueryChange(event.target.value);
        }}
        onKeyDown={(event) => {
          event.stopPropagation();
        }}
        onClick={(event) => {
          event.stopPropagation();
        }}
      />
    </ListSubheader>
  );
}

const searchListSubheaderSx: SxProps<Theme> = (theme: Theme) => ({ paddingBlock: theme.spacing(1), lineHeight: 'normal' });
