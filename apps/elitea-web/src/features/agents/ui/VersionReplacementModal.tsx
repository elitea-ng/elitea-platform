import type { KeyboardEvent, ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { LATEST_VERSION_NAME } from '@/entities/version';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { SingleSelect } from '@/shared/ui/SingleSelect';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * version/VersionReplacementModal.jsx`.
 *
 * `entities/version`'s public `LATEST_VERSION_NAME` (Wave-1 entities layer,
 * unit E1) replaces the baseline's own `@/[fsd]/entities/version/lib/
 * constants` import — same string constant, legal cross-slice import
 * (`entities/` may be imported freely by `features/`).
 *
 * `Select.SingleSelect`'s baseline API (`onValueChange`, `inputSX`/`labelSX`,
 * `maxDisplayValueLength`, `showBorder`) is replaced by this app's real
 * `shared/ui/SingleSelect` API (`onChange`, `sx` only — see that component's
 * own doc comment: baseline "substantially trimmed" from 50 props). No
 * `maxDisplayValueLength`/`showBorder` equivalent exists; dropped, not
 * replicated with an invented prop.
 *
 * `MUST EXPORT VIA PUBLIC API` — this component is named in the batch brief
 * as a confirmed future cross-feature (`entities/version`-adjacent UI)
 * consumer target for Wave-2 unit C6; exported from `../index.ts`.
 */
interface VersionReplacementOption {
  readonly id: number | string;
  readonly name: string;
  readonly created_at?: string | undefined;
}

interface VersionReplacementParent {
  readonly application_id: number | string;
  readonly version_id: number | string;
  readonly application_name: string;
  readonly version_name: string;
}

export interface VersionReplacementModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly versionName: string;
  readonly referencingParents: readonly VersionReplacementParent[] | undefined;
  readonly replacementVersions: readonly VersionReplacementOption[] | undefined;
  readonly onReplace: (versionId: number | string) => void;
  readonly isReplacing?: boolean | undefined;
  readonly defaultVersionId?: number | string | undefined;
}

function formatVersionName(version: VersionReplacementOption): string {
  if (version.name === LATEST_VERSION_NAME) return LATEST_VERSION_NAME;
  if (!version.created_at) return version.name;
  const date = new Date(version.created_at);
  if (Number.isNaN(date.getTime())) return version.name;
  const day = String(date.getDate()).padStart(2, '0');
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const year = date.getFullYear();
  return `${version.name} – ${day}.${month}.${year}`;
}

function sortReplacementVersions(
  versions: readonly VersionReplacementOption[],
  defaultVersionId: number | string | undefined,
): VersionReplacementOption[] {
  return [...versions].sort((a, b) => {
    if (a.id === defaultVersionId) return -1;
    if (b.id === defaultVersionId) return 1;
    if (a.name === LATEST_VERSION_NAME) return 1;
    if (b.name === LATEST_VERSION_NAME) return -1;
    return new Date(b.created_at ?? 0).getTime() - new Date(a.created_at ?? 0).getTime();
  });
}

export function VersionReplacementModal({
  open,
  onClose,
  versionName,
  referencingParents,
  replacementVersions,
  onReplace,
  isReplacing = false,
  defaultVersionId,
}: VersionReplacementModalProps): ReactNode {
  const [selectedVersionId, setSelectedVersionId] = useState<number | string>('');

  const sortedVersions = useMemo(
    () => (replacementVersions ? sortReplacementVersions(replacementVersions, defaultVersionId) : []),
    [replacementVersions, defaultVersionId],
  );

  const versionSelectOptions = useMemo(
    () => sortedVersions.map((item) => ({ label: formatVersionName(item), value: String(item.id) })),
    [sortedVersions],
  );

  useEffect(() => {
    if (open && sortedVersions.length > 0 && selectedVersionId === '') {
      const defaultVersion = sortedVersions.find((v) => v.id === defaultVersionId);
      const latestVersion = sortedVersions.find((v) => v.name === LATEST_VERSION_NAME);
      const versionToSelect = defaultVersion ?? latestVersion ?? sortedVersions[0];
      if (versionToSelect) setSelectedVersionId(versionToSelect.id);
    }
  }, [open, sortedVersions, selectedVersionId, defaultVersionId]);

  const handleReplace = useCallback(() => {
    if (selectedVersionId !== '') onReplace(selectedVersionId);
  }, [selectedVersionId, onReplace]);

  const handleClose = useCallback(() => {
    setSelectedVersionId('');
    onClose();
  }, [onClose]);

  const uniqueParents = useMemo(() => {
    if (!referencingParents) return [];
    const seen = new Set<string>();
    return referencingParents.filter((parent) => {
      const key = `${String(parent.application_id)}-${String(parent.version_id)}`;
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  }, [referencingParents]);

  const onSelectVersion = useCallback((value: string) => setSelectedVersionId(value), []);

  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      if (event.key === 'Enter' && selectedVersionId !== '' && !isReplacing) {
        event.preventDefault();
        handleReplace();
      }
    },
    [selectedVersionId, isReplacing, handleReplace],
  );

  return (
    <Dialog
      open={open}
      onClose={handleClose}
      onKeyDown={handleKeyDown}
      fullWidth
      slotProps={{ paper: { sx: paperSx } }}
    >
      {/* Text directly in DialogTitle (the `<h2>`; MuiDialogTitle sets
          headingMedium + text.secondary): an inner heading-variant Typography
          would nest a second `<h2>` inside it. */}
      <DialogTitle>
        {t('features.agents.versionReplacementModal.title', 'Version in use')}
      </DialogTitle>
      <DialogContent>
        <Typography
          component="div"
          variant="bodyMedium"
          color="text.secondary"
          sx={descriptionSx}
        >
          {t('features.agents.versionReplacementModal.descriptionPrefix', 'The version')}{' '}
          <Typography
            component="span"
            variant="headingSmall"
            sx={versionNameSx}
          >
            {versionName}
          </Typography>{' '}
          {t(
            'features.agents.versionReplacementModal.descriptionSuffix',
            'is currently used by the following agents/pipelines. Select a replacement version to update all references before deletion.',
          )}
        </Typography>

        <Box sx={affectedListSx}>
          <Typography
            variant="headingSmall"
            color="text.primary"
            component="div"
          >
            {t('features.agents.versionReplacementModal.affectedCount', 'Affected ({{count}}):', { count: uniqueParents.length })}
          </Typography>
          {uniqueParents.map((parent, index) => (
            <Typography
              key={`${String(parent.application_id)}-${String(parent.version_id)}-${index}`}
              variant="bodyMedium"
              color="text.secondary"
              component="div"
            >
              {`• ${parent.application_name} (${parent.version_name})`}
            </Typography>
          ))}
        </Box>
        <Box>
          <SingleSelect
            onChange={onSelectVersion}
            value={String(selectedVersionId)}
            options={versionSelectOptions}
            label={t('features.agents.versionReplacementModal.selectLabel', 'Replace with version')}
          />
        </Box>
      </DialogContent>
      <DialogActions sx={dialogActionsSx}>
        <BaseBtn
          variant="elitea"
          color="secondary"
          onClick={handleClose}
          disabled={isReplacing}
        >
          {t('features.agents.versionReplacementModal.cancel', 'Cancel')}
        </BaseBtn>
        <BaseBtn
          variant="elitea"
          color="alarm"
          onClick={handleReplace}
          disabled={selectedVersionId === '' || isReplacing}
          loading={isReplacing}
        >
          {isReplacing
            ? t('features.agents.versionReplacementModal.replacing', 'Replacing...')
            : t('features.agents.versionReplacementModal.replace', 'Replace & Delete')}
        </BaseBtn>
      </DialogActions>
    </Dialog>
  );
}

const paperSx: SxProps<Theme> = { width: '37.5rem' };
const versionNameSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.deleteAlertEntityName });
const descriptionSx: SxProps<Theme> = { marginBottom: '1rem' };
const affectedListSx: SxProps<Theme> = (theme: Theme) => ({
  marginBottom: theme.spacing(3),
  padding: theme.spacing(2),
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.border.tips}`,
  background: theme.vars.palette.background.info,
  maxHeight: '9.375rem',
  overflowY: 'auto',
});
const dialogActionsSx: SxProps<Theme> = { padding: '1.25rem', paddingTop: 0 };
