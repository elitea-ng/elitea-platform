import type { ReactNode } from 'react';

import { useMemo } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { AgentPipelineVersionSelector } from '@/features/agents';
import { skillVersionKey, SkillEditorToolbar, type SkillRecord, type SkillVersion } from '@/features/skills';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { buildVersionKeyById, defaultVersionOptionId, selectedVersionOptionId, toSkillVersionOptions } from './lib/skillVersionOptions';

/**
 * The skill editor's header: name, version dropdown, version-scoped write
 * controls (new version, set default, compare, restore, delete version —
 * #874) and the save/discard/delete/export toolbar.
 *
 * Split out of `EditSkill.tsx` to stay under the §3.4 file-length budget
 * once the version controls landed — the same reason `pages/agents` splits
 * its own editor across several files.
 *
 * #917/ELITEA-3290,3291,3293,3294,3295,3296 — the version dropdown is
 * `features/agents`' `AgentPipelineVersionSelector`, the SAME component
 * Agents and Pipelines use, not the bare MUI `<Select>` of plain `<MenuItem>`
 * rows this header drew before. That one component is what supplies the
 * checkmark on the selected row, the "Default" marker, the search box and
 * each row's creator + "MMM DD, YYYY, hh:mm AM/PM" timestamp — six reported
 * gaps with one cause.
 *
 * TWO DELIBERATE DIFFERENCES from the ask, both inherited from the shared
 * component rather than invented here:
 *  - The default version is marked with a "Default" LABEL, not a pin button
 *    inside the row. The component's own header records why: a button inside
 *    a `role="menuitem"` row is axe's `nested-interactive` (impact
 *    "serious"), which the E2E `checkA11y` fixture does not disable.
 *  - Set-default and delete-version stay the buttons this header already
 *    carries, so `onSetDefaultVersion`/`onDeleteVersion` are not passed and
 *    the menu renders no command items — the component's documented way to
 *    keep a plain list.
 */
export interface SkillEditorHeaderProps {
  readonly skill: SkillRecord;
  readonly versions: readonly SkillVersion[];
  readonly activeVersion: string | undefined;
  readonly isDirty: boolean;
  readonly isSaving: boolean;
  readonly isSettingDefault: boolean;
  readonly onNavigateVersion: (version: string) => void;
  readonly onNewVersion: () => void;
  readonly onSetDefault: (version: string) => void;
  readonly onCompare: () => void;
  readonly onRestore: () => void;
  readonly onDeleteVersion: () => void;
  readonly onSave: () => void;
  readonly onDiscard: () => void;
  readonly onDelete: () => void;
  readonly onExport: () => void;
  readonly publishing: ReactNode;
}

export function SkillEditorHeader(props: SkillEditorHeaderProps): ReactNode {
  const selectedVersion =
    props.activeVersion ?? skillVersionKey(props.skill.version_details ?? props.versions[0]!);
  const versionOptions = useMemo(() => toSkillVersionOptions(props.versions), [props.versions]);
  const versionKeyById = useMemo(() => buildVersionKeyById(props.versions), [props.versions]);
  const activeVersionObj = props.versions.find((version) => skillVersionKey(version) === selectedVersion);
  // "base" is reserved (see SkillVersion's Go doc comment) — it cannot be
  // deleted, and restoring it onto itself is a no-op the backend would
  // still accept but that offers the user nothing.
  const isNamedVersion = activeVersionObj !== undefined && activeVersionObj.name !== 'base';
  return (
    <Box sx={headerSx}>
      <Box sx={titleSx}>
        <Typography variant="headingSmall">{props.skill.name}</Typography>
        {props.versions.length > 0 && (
          <AgentPipelineVersionSelector
            versions={versionOptions}
            applicationVersionId={selectedVersionOptionId(props.versions, selectedVersion)}
            defaultVersionId={defaultVersionOptionId(props.versions)}
            onSelectVersion={(version) => {
              const key = versionKeyById.get(version.id);
              if (key !== undefined && key !== selectedVersion) props.onNavigateVersion(key);
            }}
          />
        )}
        <BaseBtn
          variant="secondary"
          startIcon={<AddOutlinedIcon />}
          onClick={props.onNewVersion}
        >
          {t('skills.edit.newVersion', 'New version')}
        </BaseBtn>
        {props.activeVersion && (
          <BaseBtn
            variant="secondary"
            disabled={props.isSettingDefault}
            onClick={() => props.onSetDefault(props.activeVersion ?? '')}
          >
            {t('skills.edit.setDefault', 'Set default')}
          </BaseBtn>
        )}
        {props.versions.length >= 2 && (
          <BaseBtn
            variant="secondary"
            data-testid="skill-compare-versions-button"
            onClick={props.onCompare}
          >
            {t('skills.edit.compareVersions', 'Compare versions')}
          </BaseBtn>
        )}
        {isNamedVersion && (
          <>
            <BaseBtn
              variant="secondary"
              data-testid="skill-restore-version-button"
              onClick={props.onRestore}
            >
              {t('skills.edit.restoreVersion', 'Restore')}
            </BaseBtn>
            <BaseBtn
              variant="secondary"
              data-testid="skill-delete-version-button"
              onClick={props.onDeleteVersion}
            >
              {t('skills.edit.deleteVersion', 'Delete version')}
            </BaseBtn>
          </>
        )}
      </Box>
      <SkillEditorToolbar
        isDirty={props.isDirty}
        isSaving={props.isSaving}
        canDelete
        onSave={props.onSave}
        onDiscard={props.onDiscard}
        onDelete={props.onDelete}
        onExport={props.onExport}
        publishing={props.publishing}
      />
    </Box>
  );
}

const headerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: theme.spacing(1),
  padding: theme.spacing(1, 3),
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});
const titleSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: 1 };
