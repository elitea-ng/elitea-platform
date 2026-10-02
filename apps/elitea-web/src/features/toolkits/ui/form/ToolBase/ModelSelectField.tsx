import { useCallback, useEffect, useMemo, type ReactNode } from 'react';

import FormControl from '@mui/material/FormControl';
import FormHelperText from '@mui/material/FormHelperText';
import InputLabel from '@mui/material/InputLabel';
import MenuItem from '@mui/material/MenuItem';
import Select, { type SelectChangeEvent } from '@mui/material/Select';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BUTTON_VARIANTS, BaseBtn } from '@/shared/ui/BaseBtn';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';

import { useToolkitModels, type ToolkitModelSection } from '../../../api/toolkitChatSession';

/**
 * The toolkit form's model picker — one component for all three of the
 * baseline's near-identical selects (`components/LlmModelSelect.jsx`,
 * `EmbeddingModelSelect.jsx`, `ImageGenerationModelSelect.jsx`). Those three
 * differ only in their `section` query param and their label; collapsing them
 * is the same "one component, one `section` prop" move
 * `useToolkitModels` already makes on the data side.
 *
 * **#308 — why this file exists.** `ToolBaseProperty.dispatch.tsx`'s
 * `renderCredentialLike` returns `null` outright when no
 * `slots.renderCredentialLikeField` is supplied, and nothing in the tree
 * supplied one. A toolkit whose schema carries a model field therefore
 * rendered a blank gap where the picker belongs, with no error and nothing
 * to click. `ToolkitForm` now supplies a default slot implementation
 * (`ToolkitForm.hooks.ts`) that routes the three model kinds here.
 *
 * **The stored value is the model NAME, not the synthesized row id.** The
 * baseline's option `value` is `model.name` (`EmbeddingModelSelect.jsx`'s
 * `newModelsMenuData`), and the Go side reads it back the same way —
 * `toolkits/handler.go:857-863` looks the saved `settings["embedding_model"]`
 * up as `data->>'name' = $1`. `id` (`${project_id}_${name}`) exists only to
 * key the list; persisting it would make every saved toolkit fail that
 * existence check.
 */
export interface ModelSelectFieldProps {
  readonly section: ToolkitModelSection;
  readonly projectId: string | undefined;
  readonly label: string;
  readonly value: unknown;
  readonly onChange: (value: string, options?: { isAutoSelect: boolean }) => void;
  readonly required?: boolean | undefined;
  readonly disabled?: boolean | undefined;
  readonly error?: boolean | undefined;
  readonly helperText?: string | undefined;
}

export function ModelSelectField({
  section,
  projectId,
  label,
  value,
  onChange,
  required,
  disabled,
  error,
  helperText,
}: ModelSelectFieldProps): ReactNode {
  const { data, isFetching, refetch } = useToolkitModels(projectId, section);

  // Derived from `data`, not from a `data?.models ?? []` local: the fallback
  // literal is a fresh array every render, which would make this `useMemo`
  // recompute every time (and trips `react-hooks/exhaustive-deps`).
  const options = useMemo(
    () => (data?.models ?? []).map((model) => ({ name: model.name, label: model.display_name ?? model.name })),
    [data],
  );

  /*
   * The saved value can name a model that is no longer in the list (deleted
   * credential, or a shared model this project lost access to). MUI logs an
   * "out-of-range value" warning and renders the trigger EMPTY in that case,
   * which is indistinguishable from "nothing selected" — so the stale name is
   * kept as its own option rather than silently dropped, the same way
   * `CredentialsSelect` keeps a `CredentialNotFoundValue` row.
   */
  const selected = typeof value === 'string' ? value : '';
  useEffect(() => {
    if (!disabled && selected === '' && data?.defaultModel) {
      onChange(data.defaultModel.name, { isAutoSelect: true });
    }
  }, [data?.defaultModel, disabled, onChange, selected]);
  const hasSelected = selected !== '' && !options.some((option) => option.name === selected);

  const handleChange = useCallback((event: SelectChangeEvent<string>) => onChange(event.target.value), [onChange]);
  const handleRefresh = useCallback(() => void refetch(), [refetch]);

  const labelId = `model-select-${section}-${label}`;

  return (
    <FormControl
      fullWidth
      error={error === true}
      required={required === true}
      disabled={disabled === true}
      sx={rootSx}
    >
      <InputLabel id={labelId}>{label}</InputLabel>
      <Select
        labelId={labelId}
        label={label}
        value={selected}
        onChange={handleChange}
        // Named for assistive tech AND for tests: the field is otherwise only
        // identifiable by its schema-derived label, which varies per toolkit.
        inputProps={{ 'data-testid': `model-select-${section}`, 'aria-label': label }}
        endAdornment={
          <Tooltip
            title={t('features.toolkits.modelSelect.refresh', 'Refresh the models')}
            placement="top"
          >
            <BaseBtn
              variant={BUTTON_VARIANTS.tertiary}
              size="small"
              onClick={handleRefresh}
              disabled={isFetching}
              sx={refreshSx}
              aria-label={t('features.toolkits.modelSelect.refresh', 'Refresh the models')}
            >
              <RefreshIcon />
            </BaseBtn>
          </Tooltip>
        }
      >
        {hasSelected && (
          <MenuItem
            value={selected}
            key={selected}
          >
            {selected}
          </MenuItem>
        )}
        {options.map((option) => (
          <MenuItem
            value={option.name}
            key={option.name}
          >
            {option.label}
          </MenuItem>
        ))}
      </Select>
      {helperText !== undefined && helperText !== '' && <FormHelperText>{helperText}</FormHelperText>}
    </FormControl>
  );
}

const rootSx: SxProps<Theme> = { marginTop: '0.5rem' };
// Clear of the dropdown arrow, which MUI positions at the same right edge.
const refreshSx: SxProps<Theme> = { marginRight: '1rem', minWidth: 'auto' };
