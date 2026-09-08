import {
  type ChangeEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  forwardRef,
} from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import type { ApplicationVersionDetail, VersionWriteRequest } from '@/shared/api/generated/model';
import { MAX_VERSION_LENGTH } from '@/shared/lib/limits';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import type { SaveNewVersionInput } from '../model/useSaveNewVersion';
import { useSaveNewVersion } from '../model/useSaveNewVersion';

/** @public Imperative escape hatch matching the baseline's `ref.current.onSaveVersion()` (`SaveNewVersionButton.jsx:34-36`), used by a caller that triggers "save as version" from elsewhere (e.g. a keyboard shortcut). */
export interface SaveNewVersionButtonHandle {
  onSaveVersion: () => void;
}

/** @public */
export interface SaveNewVersionButtonProps {
  applicationId: string;
  projectId: string | undefined;
  /** Every existing version name — used to reject a duplicate before sending the request (old app: `versions.find(item => item.name === newVersion)`, `SaveNewVersionButton.jsx:57-58`). */
  existingVersionNames: readonly string[];
  /** The current version's fields, cloned onto the new one (old app: `version_details` with `id: undefined`). */
  version: Omit<VersionWriteRequest, 'name'>;
  /** Copy skill bindings from the selected version, not the default version. */
  sourceVersionId?: number;
  disabled?: boolean;
  onClickHandler?: () => void;
  onSuccess?: (newVersion: ApplicationVersionDetail) => void;
  onError?: (message: string) => void;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Components/
 * Applications/SaveNewVersionButton.jsx`.
 *
 * DISCLOSED DEVIATIONS:
 *  - No ambient form context — `existingVersionNames`/`version` are
 *    explicit props instead of `useFormikContext()` reads, matching every
 *    sibling `features/agents/ui/*` component's convention.
 *  - Reuses this slice's own `useSaveNewVersion` (`../model/
 *    useSaveNewVersion.ts`, a sibling A1 sub-unit's already-landed port of
 *    the SAME baseline hook this button used) rather than re-deriving the
 *    POST call — see that file's own doc comment for the real,
 *    shared backend contract, including copying skills from the selected
 *    source version, and the dropped pipeline/navigation/nav-blocker orchestration.
 *  - No local `InputVersionDialog` port exists anywhere in this app yet;
 *    the name-entry dialog is rebuilt inline here on plain MUI `Dialog`
 *    components, matching the sibling `VersionReplacementModal.tsx`'s own
 *    house style for an agent-domain modal (`Dialog`/`DialogTitle`/
 *    `DialogContent`/`DialogActions`, not `shared/ui/BaseModal`). The
 *    version-name field keeps the baseline's `MAX_VERSION_LENGTH` client-side
 *    cap (`InputVersionDialog.jsx:212`, `common/constants.js:72` — 20) via
 *    `shared/lib/limits`'s already-ported `MAX_VERSION_LENGTH`.
 *  - `useToast()` replaced with `onError` receiving the already-resolved
 *    message string (via `useSaveNewVersion`'s `errorMessage`), matching
 *    every other button in this sub-unit. `onError` is invoked from a
 *    `pendingSaveRef`-gated `useEffect` on the hook's live `error`/
 *    `errorMessage` state — not from inside the `onCreateNewVersion(...)
 *    .then()` continuation — because that continuation's closure captures
 *    `errorMessage` as of the moment the button was clicked (before
 *    `useSaveNewVersion`'s internal `setError` from THIS attempt has had a
 *    chance to flush a re-render), which either misses the very first
 *    failure entirely or reports the PREVIOUS attempt's message on later
 *    ones. `pendingSaveRef` (set the moment a save is kicked off, cleared
 *    once handled) makes the effect fire exactly once per attempt, reading
 *    the error at RENDER time — the same way the old app's `useToast`-driven
 *    `toastError(buildErrorMessage(error))` always reflected the CURRENT
 *    mutation's error rather than one captured ahead of it resolving.
 */
export const SaveNewVersionButton = forwardRef<SaveNewVersionButtonHandle, SaveNewVersionButtonProps>(
  function SaveNewVersionButton(
    {
      applicationId,
      projectId,
      existingVersionNames,
      version,
      sourceVersionId,
      disabled = false,
      onClickHandler,
      onSuccess,
      onError,
    },
    ref,
  ): ReactNode {
    const [showInputVersion, setShowInputVersion] = useState(false);
    const [newVersion, setNewVersion] = useState('');
    const [duplicateNameError, setDuplicateNameError] = useState(false);

    const internalClickHandler = useCallback(() => {
      setShowInputVersion(true);
    }, []);

    const onSaveVersion = useCallback(() => {
      if (onClickHandler) {
        onClickHandler();
      } else {
        internalClickHandler();
      }
    }, [internalClickHandler, onClickHandler]);

    useImperativeHandle(ref, () => ({ onSaveVersion: internalClickHandler }), [internalClickHandler]);

    const onCancelShowInputVersion = useCallback(() => {
      setShowInputVersion(false);
      setNewVersion('');
      setDuplicateNameError(false);
    }, []);

    /** Set the moment a save is kicked off, cleared once its outcome (success or error) has been handled — see the module doc comment for why `onError` is driven off this instead of the `onCreateNewVersion(...).then()` continuation. */
    const pendingSaveRef = useRef(false);

    const handleSuccess = useCallback(
      (data: ApplicationVersionDetail) => {
        pendingSaveRef.current = false;
        setShowInputVersion(false);
        onSuccess?.(data);
      },
      [onSuccess],
    );

    const { onCreateNewVersion, isSavingNewVersion, error, errorMessage } = useSaveNewVersion({
      onSuccess: handleSuccess,
    });

    useEffect(() => {
      if (!pendingSaveRef.current || error === undefined) return;
      pendingSaveRef.current = false;
      if (errorMessage !== undefined) onError?.(errorMessage);
    }, [error, errorMessage, onError]);

    const onInputVersion = useCallback((event: ChangeEvent<HTMLInputElement>) => {
      setNewVersion(event.target.value);
      setDuplicateNameError(false);
    }, []);

    const onConfirmVersion = useCallback(() => {
      const trimmed = newVersion.trim();
      if (trimmed === '') return;
      if (existingVersionNames.includes(trimmed)) {
        setDuplicateNameError(true);
        return;
      }
      if (projectId === undefined) return;
      const input: SaveNewVersionInput = {
        projectId,
        applicationId: Number(applicationId),
        name: trimmed,
        version,
        ...(sourceVersionId === undefined ? {} : { sourceVersionId }),
      };
      pendingSaveRef.current = true;
      void onCreateNewVersion(input);
    }, [newVersion, existingVersionNames, projectId, applicationId, version, sourceVersionId, onCreateNewVersion]);

    return (
      <>
        <BaseBtn
          disabled={isSavingNewVersion || disabled}
          variant="elitea"
          color="secondary"
          onClick={onSaveVersion}
        >
          {t('agents.saveNewVersionButton.label', 'Save As Version')}
          {isSavingNewVersion && <CircularProgress size={20} />}
        </BaseBtn>
        <Dialog
          open={showInputVersion}
          onClose={onCancelShowInputVersion}
        >
          <DialogTitle>{t('agents.saveNewVersionButton.dialogTitle', 'Create version')}</DialogTitle>
          <DialogContent>
            <Box sx={{ minWidth: '20rem', paddingTop: '0.5rem' }}>
              <TextField
                fullWidth
                variant="standard"
                label={t('agents.saveNewVersionButton.versionNameLabel', 'Version name')}
                value={newVersion}
                onChange={onInputVersion}
                error={duplicateNameError}
                helperText={
                  duplicateNameError
                    ? t(
                        'agents.saveNewVersionButton.duplicateNameError',
                        'A version with that name already exists. Please pick a unique name.',
                      )
                    : undefined
                }
                slotProps={{ htmlInput: { maxLength: MAX_VERSION_LENGTH } }}
              />
            </Box>
          </DialogContent>
          <DialogActions>
            <Button onClick={onCancelShowInputVersion}>{t('agents.saveNewVersionButton.cancel', 'Cancel')}</Button>
            <Button
              variant="contained"
              disabled={newVersion.trim() === '' || isSavingNewVersion}
              onClick={onConfirmVersion}
            >
              {t('agents.saveNewVersionButton.confirm', 'Save')}
            </Button>
          </DialogActions>
        </Dialog>
      </>
    );
  },
);
