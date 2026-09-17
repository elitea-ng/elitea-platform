/**
 * AgentIconEditor — the agent editor's icon control: a clickable `EntityIcon`
 * that opens an icon picker and persists the selection immediately.
 *
 * `elitea_issues: #6627` ("[BUG] Agent header icon does not update after
 * selecting a new icon — only appears after a page reload"). Re-judged
 * against THIS app rather than the old EliteaUI report it was filed against:
 * there is no bug to reproduce here, because there was no editable icon at
 * all — `ApplicationEditForm.tsx`'s and `EntityIcon.tsx`'s own doc comments
 * both disclose it as a known, deliberate gap ("a future unit that needs an
 * EDITABLE entity icon should build that mode fresh"), and
 * `CreateAgentForm.tsx`'s `iconSlot` prop has stood empty at every call site
 * since. This component is that unit, built the way `features/skills`'
 * `SkillIconDialog.tsx` + `pages/skills/EditSkill.tsx` already solved the
 * identical problem for the sibling entity — reusing the same
 * `shared/ui/IconPickerDialog`.
 *
 * UNLIKE the skill version, this component owns its own persistence
 * (`useBindApplicationIconMutation`) rather than handing a raw
 * `onIconSelect` up to the page: `UpdateIcon` (the Go PUT) writes
 * `meta.icon_meta` directly, so there is nothing for a caller to fold into a
 * Save button — selecting an icon is its own complete, immediately-visible
 * write, exactly like the skill dialog's OWN upload path already is
 * (`SkillIconDialog.tsx`'s `handleUpload` binds in the same request). Local
 * `useState` mirrors the persisted value so the control repaints on the
 * SAME render selecting it causes, without waiting on the invalidated
 * query's refetch — that render-without-a-round-trip is the whole feature
 * #6627 was filed over.
 */
import { type ReactNode, useCallback, useMemo, useState } from 'react';

import ButtonBase from '@mui/material/ButtonBase';
import type { SxProps, Theme } from '@mui/material/styles';

import { useGetApplicationDefaultIcons } from '@/shared/api/generated/applications/applications';
import type { DefaultIcon } from '@/shared/api/generated/model/defaultIcon.zod';
import { t } from '@/shared/i18n';
import { IconPickerDialog, type PickableIcon } from '@/shared/ui/IconPickerDialog';

import {
  type ApplicationIconMeta,
  useBindApplicationIconMutation,
  useUploadApplicationIconMutation,
} from '../api/applicationIconApi';

import { EntityIcon } from './EntityIcon';

export interface AgentIconEditorProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  /** The agent VERSION the icon binds to; absent (still-unsaved draft) disables the control — `UpdateIcon` addresses a version, so there is nothing to bind to yet. */
  readonly versionId: string | undefined;
  readonly agentName: string;
  readonly iconMeta: ApplicationIconMeta | null | undefined;
  readonly disabled?: boolean | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

// R-T10 bans the literal `borderRadius: '50%'` — `radiusPill` is this app's
// token for exactly this "true circle" need (`EntityIcon.tsx` uses the same
// token for the same reason).
const buttonSx: SxProps<Theme> = (theme: Theme) => ({
  padding: 0,
  border: 'none',
  background: 'none',
  borderRadius: theme.vars.shape.radiusPill,
  cursor: 'pointer',
  '&:disabled': { cursor: 'default' },
});

export function AgentIconEditor({
  projectId,
  applicationId,
  versionId,
  agentName,
  iconMeta,
  disabled,
  sx,
}: AgentIconEditorProps): ReactNode {
  const [open, setOpen] = useState(false);
  // Mirrors the persisted value: set the instant a bind succeeds, so the icon
  // repaints on that same render rather than waiting for the invalidated
  // `getApplication` query to refetch. `undefined` means "no local override
  // yet" — fall through to the caller's own `iconMeta` (the version's stored
  // value, including across a version switch or a page reload).
  const [localIconMeta, setLocalIconMeta] = useState<ApplicationIconMeta | null | undefined>(undefined);
  const effectiveIconMeta = localIconMeta !== undefined ? localIconMeta : iconMeta;

  const resolvedProjectId = projectId ?? '';
  const { data: defaultIconsResponse, isLoading: loadingDefault } = useGetApplicationDefaultIcons(resolvedProjectId, {
    query: { enabled: open && !!projectId },
  });
  const defaultIcons = useMemo(
    () => (defaultIconsResponse?.data ?? []) as DefaultIcon[],
    [defaultIconsResponse],
  );

  const uploadMutation = useUploadApplicationIconMutation(resolvedProjectId);
  const bindMutation = useBindApplicationIconMutation(resolvedProjectId);

  const canBind = projectId !== undefined && applicationId !== undefined && versionId !== undefined;

  const applyIcon = useCallback(
    async (next: ApplicationIconMeta | null): Promise<void> => {
      if (!canBind || projectId === undefined || applicationId === undefined || versionId === undefined) return;
      await bindMutation.mutateAsync({ applicationId, versionId, iconMeta: next });
      setLocalIconMeta(next);
    },
    [bindMutation, canBind, projectId, applicationId, versionId],
  );

  const handleSelect = useCallback(
    (name: string | null) => {
      if (name === null) {
        void applyIcon(null);
        return;
      }
      const chosen: PickableIcon | undefined = defaultIcons.find((icon) => icon.name === name);
      if (!chosen?.url) return; // a name with no url cannot be bound — the server requires both.
      void applyIcon({ name: chosen.name, url: chosen.url });
    },
    [applyIcon, defaultIcons],
  );

  const handleUpload = useCallback(
    async (file: File): Promise<void> => {
      const uploaded = await uploadMutation.mutateAsync({ file });
      if (!uploaded) return; // the "no file" fast path — nothing to bind.
      await applyIcon(uploaded);
    },
    [applyIcon, uploadMutation],
  );

  const handleClose = useCallback(() => setOpen(false), []);
  const handleOpen = useCallback(() => setOpen(true), []);
  // The uploaded-icon gallery is always empty (see `uploadedIcons={[]}`
  // below), so the picker never actually offers a delete action — this
  // satisfies `IconPickerDialogProps.onDeleteIcon`'s required shape without
  // a real target to delete.
  const handleDelete = useCallback((_name: string) => Promise.resolve(), []);

  return (
    <>
      <ButtonBase
        component="button"
        type="button"
        onClick={handleOpen}
        disabled={disabled || !canBind}
        aria-label={t('agents.entityIcon.editAlt', 'Change agent icon')}
        data-testid="agent-icon-edit-button"
        sx={buttonSx}
      >
        <EntityIcon
          icon={effectiveIconMeta?.url ? { url: effectiveIconMeta.url } : undefined}
          entityType="agent"
          sx={sx}
        />
      </ButtonBase>
      <IconPickerDialog
        open={open}
        onClose={handleClose}
        selectedIcon={effectiveIconMeta ? { name: effectiveIconMeta.name ?? '', url: effectiveIconMeta.url } : null}
        placeholderName={agentName}
        defaultIcons={defaultIcons}
        loadingDefaultIcons={loadingDefault}
        uploadedIcons={[]}
        // The uploaded-icon GALLERY is a separate, still-stubbed Go route
        // (`ListUploadedIcons` always answers `{rows: [], total: 0}` —
        // `handler.go:2725`'s own comment says so) — out of this issue's
        // scope. A fresh upload still works (the picker's own "Upload" tab
        // calls `onUpload` directly); only re-picking a PREVIOUSLY uploaded
        // icon from a gallery is unavailable, same as the skill dialog's
        // identical `getApplicationIcons` stub leaves for that entity.
        onSelectIcon={handleSelect}
        onUpload={(file) => handleUpload(file)}
        onDeleteIcon={handleDelete}
      />
    </>
  );
}
