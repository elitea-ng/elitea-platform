import { useState, type ReactNode } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlineOutlined';
import DownloadOutlinedIcon from '@mui/icons-material/DownloadOutlined';
import MoreVertIcon from '@mui/icons-material/MoreVert';
import IconButton from '@mui/material/IconButton';
import ListItemIcon from '@mui/material/ListItemIcon';
import ListItemText from '@mui/material/ListItemText';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';

import { t } from '@/shared/i18n';

import type { SkillRecord } from '../model/types';

export interface SkillRowMenuProps {
  readonly skill: SkillRecord;
  readonly onExport: (skill: SkillRecord) => void;
  readonly onDelete: (skill: SkillRecord) => void;
}

/**
 * The three-dot menu in the table view's Actions column (#2114): the same
 * Export and Delete the card carries in its corner. Moving to a folder stays in
 * the Folder column beside it, which already holds that control.
 */
export function SkillRowMenu({ skill, onExport, onDelete }: SkillRowMenuProps): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const label = t('skills.list.actionsFor', 'Actions for {{name}}', { name: skill.name });
  const close = (): void => setAnchor(null);
  return (
    <>
      <IconButton
        size="small"
        aria-label={label}
        aria-haspopup="menu"
        data-testid="skill-row-menu"
        onClick={(event) => setAnchor(event.currentTarget)}
      >
        <MoreVertIcon fontSize="small" />
      </IconButton>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={close}
      >
        <MenuItem
          onClick={() => {
            close();
            onExport(skill);
          }}
        >
          <ListItemIcon>
            <DownloadOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t('skills.list.export', 'Export')}</ListItemText>
        </MenuItem>
        <MenuItem
          onClick={() => {
            close();
            onDelete(skill);
          }}
        >
          <ListItemIcon>
            <DeleteOutlineIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t('skills.list.delete', 'Delete')}</ListItemText>
        </MenuItem>
      </Menu>
    </>
  );
}
