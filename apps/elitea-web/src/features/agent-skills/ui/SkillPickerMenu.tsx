import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { AgentSkill } from '../api/agentSkillsApi';

/**
 * The "+ Skill" picker, as production renders it: a search box, then the
 * project's skills.
 *
 * Split out of `AgentSkillsPanel` to keep that component inside the §3.5
 * complexity budget, and because the version-resolution rule below is worth
 * having in one readable place.
 */
export interface SkillPickerMenuProps {
  readonly anchorEl: HTMLElement | null;
  readonly query: string;
  readonly onQueryChange: (next: string) => void;
  readonly options: readonly AgentSkill[];
  readonly isLoading: boolean;
  /** Already-attached skills are shown and disabled, not hidden. */
  readonly attachedIds: ReadonlySet<number>;
  readonly onClose: () => void;
  readonly onPick: (skillId: number, skillVersionId: number) => void;
}

/**
 * The version the attachment will carry.
 *
 * `skill_version_id` is required by the attach route, and an attachment
 * without one is a row the agent run drops (see `../api/agentSkillsApi.ts`).
 * The list route names the skill's own default version in `version_id`; when
 * it does not, the first entry of `versions` is the `base` version every skill
 * has.
 */
function pickVersionId(skill: AgentSkill): number | undefined {
  return skill.version_id ?? skill.versions?.[0]?.id;
}

export function SkillPickerMenu({
  anchorEl,
  query,
  onQueryChange,
  options,
  isLoading,
  attachedIds,
  onClose,
  onPick,
}: SkillPickerMenuProps): ReactNode {
  return (
    <Menu
      anchorEl={anchorEl}
      open={anchorEl !== null}
      onClose={onClose}
      slotProps={{ list: { 'aria-label': t('features.agentSkills.pickerLabel', 'Skills') } }}
    >
      {/*
        `onKeyDown` is stopped here, and it is load-bearing. MUI's `MenuList`
        implements type-ahead: a printable key moves focus to the item whose
        label starts with it. Without this guard every letter typed into the
        search box is eaten by that handler, the field stays empty and the
        search silently does nothing — which is exactly how a picker looks when
        it works and exactly how it looks when it does not.
      */}
      <Box
        sx={searchSx}
        onKeyDown={(event) => event.stopPropagation()}
      >
        <TextField
          size="small"
          value={query}
          placeholder={t('features.agentSkills.search', 'Search skills...')}
          onChange={(event) => onQueryChange(event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'agent-skill-search' } }}
          fullWidth
        />
      </Box>
      {options.length === 0 && (
        <MenuItem
          disabled
          data-testid="agent-skill-picker-empty"
        >
          {isLoading
            ? t('features.agentSkills.loading', 'Loading skills...')
            : t('features.agentSkills.none', 'No skills available')}
        </MenuItem>
      )}
      {options.map((skill) => {
        const versionId = pickVersionId(skill);
        return (
          <MenuItem
            key={skill.id}
            data-testid={`agent-skill-option-${String(skill.id)}`}
            disabled={attachedIds.has(skill.id) || versionId === undefined}
            onClick={() => {
              if (versionId !== undefined) onPick(skill.id, versionId);
            }}
          >
            {skill.name}
          </MenuItem>
        );
      })}
    </Menu>
  );
}

const searchSx: SxProps<Theme> = { padding: '0.5rem 0.75rem', minWidth: '16rem' };
