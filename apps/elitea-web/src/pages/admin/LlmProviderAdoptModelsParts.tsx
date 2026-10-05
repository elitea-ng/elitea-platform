/**
 * The adopt-models dialog's presentational parts: its alert stack, its kind
 * field, its selection bar and its list.
 *
 * Their own file so `./LlmProviderAdoptModelsDialog.tsx` stays the dialog's
 * STATE — what is selected, what is being adopted, what failed — and these stay
 * markup with one decision each. The same split `LlmProviderTableParts.tsx`
 * makes for the providers table, and the same one the §3.5 complexity budget
 * forces once a dialog grows more than a couple of conditional banners.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import DialogActions from '@mui/material/DialogActions';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import List from '@mui/material/List';
import ListItem from '@mui/material/ListItem';
import ListItemText from '@mui/material/ListItemText';
import MenuItem from '@mui/material/MenuItem';
import Paper from '@mui/material/Paper';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { platformModelTypeLabel, type PlatformModel } from './api/adminLlmPlatformModelsApi';

/** One id that could not be adopted, and the server's own reason for it. */
export interface AdoptFailure {
  readonly id: string;
  readonly reason: string;
}

/**
 * The ids this platform already publishes, by BOTH names a row can carry.
 *
 * `elitea_title` is what a caller addresses and is UNIQUE per project;
 * `model_name` is the provider's own string. Adoption writes the same id into
 * both, but a hand-made row may hold it in either, and a clash on EITHER is a
 * row this dialog must not try to create — the create would be refused, not
 * duplicated.
 */
export function adoptedModelIDs(items: readonly PlatformModel[]): ReadonlySet<string> {
  const ids = new Set<string>();
  for (const item of items) {
    if (item.elitea_title !== '') ids.add(item.elitea_title);
    if (item.model_name !== '') ids.add(item.model_name);
  }
  return ids;
}

/** The three things that can be worth saying at once, each its own statement. */
export function AdoptModelAlerts({
  loadError,
  truncated,
  failures,
}: {
  readonly loadError: string | undefined;
  readonly truncated: boolean;
  readonly failures: readonly AdoptFailure[];
}): ReactNode {
  return (
    <>
      {loadError !== undefined ? (
        <Alert severity="error" data-testid="adopt-models-load-error">
          {loadError}
        </Alert>
      ) : null}

      {/* Stated, because a short list otherwise reads as the provider's whole
          catalogue — and an operator then concludes a model is not offered
          when it simply was not reached. */}
      {truncated ? (
        <Alert severity="info" data-testid="adopt-models-truncated">
          {t(
            'pages.admin.adoptModels.truncated',
            'This provider lists more models than one page can carry. The rest are not shown; add them by hand if you need them.',
          )}
        </Alert>
      ) : null}

      {/* Named one by one. A partial adoption reported as a single "some
          failed" leaves the operator to work out which, against a list they
          can no longer see. */}
      {failures.length > 0 ? (
        <Alert severity="error" data-testid="adopt-models-failures">
          {t('pages.admin.adoptModels.failed', 'These models were not adopted: {{details}}', {
            details: failures.map((failure) => `${failure.id} (${failure.reason})`).join('; '),
          })}
        </Alert>
      ) : null}
    </>
  );
}

/**
 * The kind every model in this batch is adopted as.
 *
 * A provider listing gives NAMES ONLY, so the kind cannot be read from them —
 * and a model filed under the wrong (section, type) pair is invisible to every
 * dispatch path while looking complete in the table. Asking is the honest
 * option; guessing from a substring is not.
 */
export function AdoptKindField({
  value,
  modelTypes,
  disabled,
  onChange,
}: {
  readonly value: string;
  readonly modelTypes: readonly string[];
  readonly disabled: boolean;
  readonly onChange: (value: string) => void;
}): ReactNode {
  return (
    <TextField
      select
      size="small"
      label={t('pages.admin.adoptModels.kind', 'Adopt as')}
      value={value}
      onChange={(event) => onChange(event.target.value)}
      disabled={disabled}
      slotProps={{ htmlInput: { 'data-testid': 'adopt-models-kind' } }}
      helperText={t(
        'pages.admin.adoptModels.kindHelp',
        'A provider lists names only, so the kind cannot be read from them. Adopt models of another kind in a second pass.',
      )}
    >
      {modelTypes.map((type) => (
        <MenuItem key={type} value={type}>
          {platformModelTypeLabel(type)}
        </MenuItem>
      ))}
    </TextField>
  );
}

/**
 * The listing's own progress bar.
 *
 * A component rather than an inline conditional, so the dialog itself stays
 * markup with no branches in it — the §3.5 complexity budget, which this
 * dialog crossed twice while it was being written.
 */
export function AdoptProgress({ isPending }: { readonly isPending: boolean }): ReactNode {
  if (!isPending) return null;
  return (
    <LinearProgress
      aria-label={t('pages.admin.adoptModels.loading', 'Reading the provider’s models')}
    />
  );
}

/** Cancel and Adopt, with the one label that changes while a run is in flight. */
export function AdoptDialogActions({
  adopting,
  canAdopt,
  onCancel,
  onAdopt,
}: {
  readonly adopting: boolean;
  readonly canAdopt: boolean;
  readonly onCancel: () => void;
  readonly onAdopt: () => void;
}): ReactNode {
  return (
    <DialogActions>
      <Button variant="elitea" color="tertiary" onClick={onCancel} disabled={adopting}>
        {t('pages.admin.adoptModels.cancel', 'Cancel')}
      </Button>
      <Button
        variant="elitea"
        color="primary"
        data-testid="adopt-models-submit"
        disabled={!canAdopt || adopting}
        onClick={onAdopt}
      >
        {adopting
          ? t('pages.admin.adoptModels.adopting', 'Adopting…')
          : t('pages.admin.adoptModels.adopt', 'Adopt selected')}
      </Button>
    </DialogActions>
  );
}

/** Select-every-new-model, and how many of them are picked. */
export function AdoptSelectionBar({
  selectable,
  selected,
  disabled,
  onSelectAll,
}: {
  readonly selectable: readonly string[];
  readonly selected: readonly string[];
  readonly disabled: boolean;
  readonly onSelectAll: (all: boolean) => void;
}): ReactNode {
  return (
    <Box sx={{ display: 'flex', gap: '1rem', alignItems: 'center', flexWrap: 'wrap' }}>
      <FormControlLabel
        control={
          <BaseCheckbox
            size="small"
            data-testid="adopt-models-select-all"
            checked={selectable.length > 0 && selected.length === selectable.length}
            indeterminate={selected.length > 0 && selected.length < selectable.length}
            disabled={selectable.length === 0 || disabled}
            onChange={(event) => onSelectAll(event.target.checked)}
          />
        }
        label={t('pages.admin.adoptModels.selectAll', 'Select every new model')}
      />
      <Typography variant="bodySmall" color="text.secondary" data-testid="adopt-models-count">
        {t('pages.admin.adoptModels.count', '{{selected}} of {{total}} new', {
          selected: selected.length,
          total: selectable.length,
        })}
      </Typography>
    </Box>
  );
}

/**
 * The provider's ids, each with its box.
 *
 * An id already published is shown CHECKED AND DISABLED rather than hidden: an
 * operator hunting for a model has to be able to see that it is already there,
 * because an absent row reads as "this provider does not offer it".
 */
export function AdoptModelList({
  models,
  adopted,
  selected,
  disabled,
  onToggle,
  isPending,
  failed,
}: {
  readonly models: readonly string[];
  readonly adopted: ReadonlySet<string>;
  readonly selected: readonly string[];
  readonly disabled: boolean;
  readonly onToggle: (id: string) => void;
  readonly isPending: boolean;
  readonly failed: boolean;
}): ReactNode {
  if (isPending) return null;
  if (models.length === 0) {
    // The empty state is suppressed when the read FAILED: "this provider
    // offers no models" and "the listing could not be read" are different
    // facts, and only the first is one an operator would act on.
    if (failed) return null;
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="adopt-models-empty">
        {t(
          'pages.admin.adoptModels.empty',
          'This provider listed no models. Check that the credential has access to them, or add a model by hand.',
        )}
      </Typography>
    );
  }

  return (
    <Paper variant="outlined" sx={{ maxHeight: '18rem', overflowY: 'auto' }}>
      <List dense data-testid="adopt-models-list">
        {models.map((id) => (
          <AdoptModelRow
            key={id}
            id={id}
            already={adopted.has(id)}
            checked={selected.includes(id)}
            disabled={disabled}
            onToggle={onToggle}
          />
        ))}
      </List>
    </Paper>
  );
}

/** One id's row. Split out so the list itself stays a map with no branches. */
function AdoptModelRow({
  id,
  already,
  checked,
  disabled,
  onToggle,
}: {
  readonly id: string;
  readonly already: boolean;
  readonly checked: boolean;
  readonly disabled: boolean;
  readonly onToggle: (id: string) => void;
}): ReactNode {
  return (
    <ListItem disablePadding sx={{ paddingLeft: '0.5rem' }}>
      <BaseCheckbox
        size="small"
        data-testid={`adopt-models-item-${id}`}
        checked={already || checked}
        disabled={already || disabled}
        onChange={() => onToggle(id)}
        slotProps={{ input: { 'aria-label': id } }}
      />
      <ListItemText
        primary={id}
        secondary={
          already ? t('pages.admin.adoptModels.alreadyPublished', 'Already published') : undefined
        }
        slotProps={{ primary: { variant: 'bodyMedium' }, secondary: { variant: 'bodySmall' } }}
      />
    </ListItem>
  );
}
