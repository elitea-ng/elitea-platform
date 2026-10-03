import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useSearch } from '@tanstack/react-router';

import type { Bucket } from '@/entities/bucket';
import { useArtifactBuckets, useArtifactMutations } from '@/features/artifacts';
import { t } from '@/shared/i18n';

import { useSelectedProjectId } from './lib/useSelectedProjectId';

const BUCKET_NAME_PATTERN = /^[a-zA-Z][a-zA-Z0-9-]*$/;

export function validateBucketName(name: string): string {
  if (name.trim() === '') return 'Name is required.';
  if (name.length > 56) return 'Name must not exceed 56 characters.';
  if (!BUCKET_NAME_PATTERN.test(name)) {
    return 'Start with a letter and use only letters, numbers, and hyphens.';
  }
  return '';
}

/**
 * Retention is optional. An empty box means "keep objects indefinitely" —
 * the `null` the API models as no lifecycle — so only a NON-empty value is
 * validated, and it must be a whole number of days above zero (the legacy
 * form's own `min(1, 'Retention value should be greater than 0')`).
 */
export function validateRetentionDays(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed === '') return '';
  const parsed = Number(trimmed);
  if (!Number.isInteger(parsed)) return 'Retention must be a whole number of days.';
  if (parsed < 1) return 'Retention must be greater than 0.';
  return '';
}

/** `''` -> `null` (no lifecycle); otherwise the parsed day count. */
export function parseRetentionDays(raw: string): number | null {
  const trimmed = raw.trim();
  return trimmed === '' ? null : Number(trimmed);
}

/** `?bucket=` -> the bucket being edited, or `undefined` for create mode. */
function editedBucketName(raw: string | undefined): string | undefined {
  return raw !== undefined && raw !== '' ? raw : undefined;
}

/** The form's heading across both modes. */
function formTitle(isEditing: boolean): string {
  return isEditing ? t('artifacts.edit.title', 'Edit bucket') : t('artifacts.create.title', 'New bucket');
}

/** The Name field's helper line. Never empty — see the #138 note at the call site. */
function nameHelperText(isEditing: boolean, touched: boolean, validationError: string): string {
  if (isEditing) return t('artifacts.edit.nameHelp', 'A bucket cannot be renamed after it is created.');
  if (touched && validationError !== '') return validationError;
  return t('artifacts.create.nameHelp', 'Bucket names can contain letters, numbers, and hyphens.');
}

/** The submit button's label across both modes and their pending states. */
function submitLabel(isEditing: boolean, pending: boolean): string {
  if (isEditing) {
    return pending ? t('artifacts.edit.saving', 'Saving…') : t('artifacts.edit.submit', 'Save bucket');
  }
  return pending ? t('artifacts.create.creating', 'Creating…') : t('artifacts.create.submit', 'Create bucket');
}

/**
 * The bucket form, in BOTH its modes.
 *
 * `?bucket=<name>` switches it to edit mode — see this route's own file for
 * why editing arrives on the create path rather than at the baseline's
 * declared-but-never-mounted `/artifacts/edit-bucket`.
 *
 * **Edit does not rename.** The name field is read-only in edit mode
 * because neither API can rename a bucket: `PUT /api/v2/artifacts/buckets/
 * {projectID}/{name}` accepts `retention_days` and `is_pinned` only, and
 * the legacy `editBucket` it replaced merely reconfigured the S3 lifecycle
 * (`expiration_measure`/`expiration_value` -> `configure_bucket_lifecycle`).
 * An editable name box here would be a control that silently discards what
 * the user typed. Retention is therefore the whole of "edit a bucket", and
 * `retention_days` is the field the create path cannot set at all (`POST
 * /buckets` takes `{name}`), which is exactly why the affordance is worth
 * having.
 */
/**
 * The retention box, seeded from the server ONCE — when the bucket row first
 * resolves. Re-seeding on every list refetch would wipe whatever the user had
 * typed since. Its own function so `CreateBucket` stays inside the §3.5
 * cyclomatic-complexity budget.
 */
function useSeededRetention(
  isEditing: boolean,
  bucket: Bucket | undefined,
): [string, (value: string) => void] {
  const [retention, setRetention] = useState('');
  const [seeded, setSeeded] = useState(false);

  useEffect(() => {
    if (!isEditing || seeded || bucket === undefined) return;
    setRetention(bucket.retentionDays === null ? '' : String(bucket.retentionDays));
    setSeeded(true);
  }, [bucket, isEditing, seeded]);

  return [retention, setRetention];
}

/**
 * The retention box. Its own component so `CreateBucket` stays inside the
 * §3.5 cyclomatic-complexity budget — it carries the field's own error/hint
 * branch, which the page does not otherwise need.
 */
function RetentionField(props: { value: string; error: string; onChange: (value: string) => void }): ReactNode {
  const { value, error, onChange } = props;
  return (
    <TextField
      fullWidth
      label={t('artifacts.edit.retention', 'Retention (days)')}
      value={value}
      error={error !== ''}
      helperText={error === '' ? t('artifacts.edit.retentionHelp', 'Leave empty to keep files indefinitely.') : error}
      onChange={(event) => onChange(event.target.value)}
    />
  );
}

export function CreateBucket(): ReactNode {
  const navigate = useNavigate();
  // `strict: false` — this app's convention for reading route search off a
  // page component rather than a route file (see `Artifacts.tsx:58`).
  const search = useSearch({ strict: false }) as { readonly bucket?: string };
  const projectId = useSelectedProjectId();
  const mutations = useArtifactMutations(projectId);

  const editingName = editedBucketName(search.bucket);
  const isEditing = editingName !== undefined;

  // Edit mode has no single-bucket GET wired in this app; the list query is
  // already cached by the page that linked here, so the current retention
  // comes off that row rather than adding a second endpoint.
  const buckets = useArtifactBuckets(isEditing ? projectId : undefined);
  const editedBucket = buckets.data?.find((bucket) => bucket.name === editingName);

  const [name, setName] = useState(editingName ?? 'new-bucket');
  const [retention, setRetention] = useSeededRetention(isEditing, editedBucket);
  const [touched, setTouched] = useState(false);
  const [error, setError] = useState<string>();
  const validationError = isEditing ? '' : validateBucketName(name);
  const retentionError = validateRetentionDays(retention);

  const pending = isEditing ? mutations.editBucketRetention.isPending : mutations.createBucket.isPending;

  const goToBucket = (bucket: string): void => {
    void navigate({ to: '/artifacts', search: { bucket, file: '', folder: '', shared_bucket: '' }, replace: true });
  };

  const submit = (): void => {
    setTouched(true);
    if (validationError !== '' || retentionError !== '' || projectId === undefined) return;
    setError(undefined);
    if (editingName !== undefined) {
      void mutations.editBucketRetention
        .mutateAsync({ name: editingName, retentionDays: parseRetentionDays(retention) })
        .then(() => goToBucket(editingName))
        .catch(() => setError('Failed to update the bucket.'));
      return;
    }
    void mutations.createBucket
      .mutateAsync(name.trim())
      .then(() => goToBucket(name.trim()))
      .catch(() => setError('Failed to create the bucket.'));
  };

  return (
    <Box sx={rootSx}>
      <Box
        component="form"
        sx={formSx}
        onSubmit={(event) => {
          event.preventDefault();
          submit();
        }}
      >
        <Typography variant="headingLarge" component="h1">{formTitle(isEditing)}</Typography>
        {error !== undefined && <Typography role="alert">{error}</Typography>}
        <TextField
          fullWidth
          label={t('common.name', 'Name')}
          value={name}
          slotProps={{ htmlInput: { maxLength: 56, readOnly: isEditing } }}
          error={touched && validationError !== ''}
          // The helper line must never collapse. It used to hold the long
          // hint until the field blurred and '' afterwards, so FormHelperText
          // vanished and the button row jumped up 22.9px — and the FIRST
          // click after typing IS that blur, so mousedown landed on the
          // button while mouseup landed 23px below it and no click event was
          // ever generated (#138). Falling back to the hint instead of ''
          // both reserves the line and keeps the naming rule on screen.
          helperText={nameHelperText(isEditing, touched, validationError)}
          onBlur={() => setTouched(true)}
          onChange={(event) => setName(event.target.value)}
        />
        {isEditing ? (
          <RetentionField
            value={retention}
            error={retentionError}
            onChange={setRetention}
          />
        ) : (
          <Typography variant="bodySmall">
            {t(
              'artifacts.create.retentionHelp',
              'Retention settings are managed by the storage service and are not part of the current bucket API.',
            )}
          </Typography>
        )}
        <Box sx={{ display: 'flex', gap: 1 }}>
          <Button
            type="submit"
            variant="contained"
            disabled={pending || validationError !== '' || retentionError !== ''}
          >
            {submitLabel(isEditing, pending)}
          </Button>
          <Button onClick={() => void navigate({ to: '/artifacts' })}>{t('common.cancel', 'Cancel')}</Button>
        </Box>
      </Box>
    </Box>
  );
}

const rootSx: SxProps<Theme> = (theme) => ({
  height: '100%',
  display: 'flex',
  justifyContent: 'center',
  padding: theme.spacing(3),
  backgroundColor: theme.vars.palette.background.tabPanel,
});
const formSx: SxProps<Theme> = (theme) => ({
  width: 'min(32rem, 100%)',
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
});
