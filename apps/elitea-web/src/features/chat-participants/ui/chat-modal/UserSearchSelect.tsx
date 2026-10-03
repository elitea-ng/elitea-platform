// @ts-nocheck
/**
 * UserSearchSelect — user-type-only entity picker for the add-participant flow.
 *
 * Ported from `pages/NewChat/AddNewUser/UserSearchSelect.jsx`.
 *
 * Cross-cutting: uses `entities/participant`'s `useFilteredEntityItems` for
 * user candidate filtering (issue #33, item 8).
 */
import { memo, useCallback, useState } from 'react';

import { useRouteContext } from '@tanstack/react-router';

import { Autocomplete, Box, TextField, Typography } from '@mui/material';

import { useFilteredEntityItems } from '@/entities/participant';

import { t } from '@/shared/i18n';

/**
 * "Currently selected project id" — local duplicate of `features/chat-input/
 * api/useSelectedProjectId.ts` (and every other Wave-2 feature slice's own
 * copy), NOT an import of it: `no-sideways-features` forbids one
 * `features/*` slice importing another, and no `shared`/`entities`
 * primitive for "the selected project id" exists yet.
 *
 * `@/shared/config` (this file's previous import source) only exports
 * `getConfig`/`Config`/`MissingEnvPage` — it has never exported a
 * `useSelectedProjectId` hook, so that import silently failed to resolve at
 * module load (a real, pre-existing defect independent of the confirmed C5
 * findings this unit fixes).
 */
function useSelectedProjectId(): string | undefined {
  const context: unknown = useRouteContext({ strict: false });
  if (typeof context !== 'object' || context === null) return undefined;
  const auth = (context as { readonly auth?: { readonly getSelectedProjectId?: () => string | undefined } }).auth;
  return auth?.getSelectedProjectId?.();
}

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

export interface UserSearchSelectProps {
  value?: Record<string, unknown> | null;
  onChange: (value: Record<string, unknown> | null) => void;
  placeholder?: string;
  disabled?: boolean;
  /** Filter by entity types to include. */
  types?: string[];
}

/**
 * UserSearchSelect component — autocomplete for selecting a user participant.
 * Uses the entity hook for filtered candidate browsing.
 */
const UserSearchSelect = memo((props: UserSearchSelectProps): React.ReactElement => {
  const { value, onChange, placeholder = t('chat-participants.search.placeholder', 'Search users...'), disabled, types } = props;

  const projectId = useSelectedProjectId();
  const [inputValue, setInputValue] = useState('');

  // Use the entity hook for filtered candidates
  const { participants: candidates, isLoading } = useFilteredEntityItems({
    projectId,
    query: inputValue,
    types: types as unknown as Parameters<typeof useFilteredEntityItems>[0]['types'],
    enabled: !!inputValue && inputValue.length >= 2,
  });

  const handleChange = useCallback(
    (_event: React.SyntheticEvent, newValue: Record<string, unknown> | null) => {
      onChange(newValue);
    },
    [onChange],
  );

  return (
    <Autocomplete
      value={value ?? null}
      onChange={handleChange}
      inputValue={inputValue}
      onInputChange={(_e, newInputValue) => setInputValue(newInputValue)}
      options={candidates}
      getOptionLabel={(option) => option.entity_meta?.name || option.meta?.user_name || t('chat-participants.common.unknown', 'Unknown')}
      isOptionEqualToValue={(option, value) => option.id === value.id}
      loading={isLoading}
      disabled={disabled}
      renderInput={(params) => (
        <TextField
          {...params}
          placeholder={placeholder}
          size="small"
          autoComplete="off"
          InputProps={{
            ...params.InputProps,
            endAdornment: (
              <>
                {isLoading ? <Box sx={{ mr: 1 }}><Typography variant="bodyMedium" component="p">{t('chat-participants.modal.loading', 'Loading...')}</Typography></Box> : null}
                {params.InputProps.endAdornment}
              </>
            ),
          }}
        />
      )}
      renderOption={(props, option) => {
        const { key, ...optionProps } = props as React.HTMLProps<HTMLLIElement>;
        return (
          <li key={key ?? option.id} {...optionProps}>
            <Typography variant="bodyMedium" component="p">{option.entity_meta?.name || option.meta?.user_name || t('chat-participants.common.unknown', 'Unknown')}</Typography>
          </li>
        );
      }}
    />
  );
});

UserSearchSelect.displayName = 'UserSearchSelect';

export default UserSearchSelect;
