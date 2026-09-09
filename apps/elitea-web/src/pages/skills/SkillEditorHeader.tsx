import type { ReactNode } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { skillVersionKey, SkillEditorToolbar, type SkillRecord, type SkillVersion } from '@/features/skills';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

/**
 * The skill editor's header: name, version dropdown, version-scoped write
 * controls (new version, set default, compare, restore, delete version —
 * #874) and the save/discard/delete/export toolbar.
 *
 * Split out of `EditSkill.tsx` to stay under the §3.4 file-length budget
 * once the version controls landed — the same reason `pages/agents` splits
 * its own editor across several files.
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
          <Select
            size="small"
            value={selectedVersion}
            onChange={(event) => props.onNavigateVersion(event.target.value)}
          >
            {props.versions.map((version) => (
              <MenuItem
                key={skillVersionKey(version)}
                value={skillVersionKey(version)}
              >
                {version.name}
                {version.is_default ? ` (${t('skills.edit.default', 'default')})` : ''}
              </MenuItem>
            ))}
          </Select>
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
