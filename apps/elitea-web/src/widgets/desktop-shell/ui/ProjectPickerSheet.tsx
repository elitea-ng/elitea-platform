/**
 * Every project a folder can be bound to, as a sheet: where the context
 * menu's "More projects…" lands (the menu itself holds only the first few).
 */
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import MenuList from '@mui/material/MenuList';

import { t } from '@/shared/i18n';

import { bindable } from '../lib/folderMenu';

export interface ProjectPickerSheetProps {
  open: boolean;
  projects: readonly { id: number; name: string; suspended?: boolean }[];
  currentId: number | null;
  onPick: (projectId: number) => void;
  onClose: () => void;
}

export function ProjectPickerSheet({ open, projects, currentId, onPick, onClose }: ProjectPickerSheetProps): React.JSX.Element {
  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="xs" aria-labelledby="project-picker-title">
      <DialogTitle id="project-picker-title">{t('workspace.path.project', 'Project')}</DialogTitle>
      <DialogContent>
        <MenuList dense disablePadding aria-label={t('workspace.project', 'Project')} sx={{ maxHeight: 360, overflowY: 'auto' }}>
          {bindable(projects).map((project) => (
            <MenuItem
              key={project.id}
              role="menuitemradio"
              aria-checked={project.id === currentId}
              onClick={() => {
                if (project.id !== currentId) onPick(project.id);
                onClose();
              }}
            >
              <Box component="span" sx={{ width: 18, display: 'inline-flex' }}>
                {project.id === currentId && <CheckOutlinedIcon fontSize="inherit" />}
              </Box>
              {project.name}
            </MenuItem>
          ))}
        </MenuList>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('workspace.index.close', 'Close')}</Button>
      </DialogActions>
    </Dialog>
  );
}
