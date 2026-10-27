/**
 * The sidebar's one project switcher, under the search: everything below it —
 * the Local work folders, the chats, applications, artifacts and the
 * Workshop — belongs to the project it shows. A square with the project's
 * initial, its name and a chevron; the picker (`ProjectPicker.tsx`) searches
 * every project the person can work in, the current one checked.
 */
import { useState } from 'react';

import UnfoldMoreIcon from '@mui/icons-material/UnfoldMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';

import { ProjectPicker } from './ProjectPicker';

export interface ProjectSwitcherProps {
  readonly projects: readonly Project[];
  readonly selectedProjectId: string | undefined;
  readonly onSelectProject: (projectId: string, projectName: string) => void;
}

export function ProjectSwitcher({ projects, selectedProjectId, onSelectProject }: ProjectSwitcherProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const current = projects.find((project) => String(project.id) === selectedProjectId);
  const name = current?.name ?? '';
  return (
    <>
      <ButtonBase
        data-testid="shell-project-switcher"
        aria-haspopup="listbox"
        aria-expanded={anchor !== null}
        aria-label={t('desktop.shell.eliteaProject', 'Project: {{name}}', { name })}
        onClick={(event) => setAnchor(event.currentTarget)}
        sx={(theme: Theme) => ({
          width: '100%',
          justifyContent: 'flex-start',
          gap: 1,
          paddingX: 1,
          paddingY: 0.5,
          borderRadius: theme.vars.shape.radiusSm,
          color: theme.vars.palette.text.secondary,
          '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
        })}
      >
        <Box
          aria-hidden
          sx={(theme: Theme) => ({
            width: '1.25rem',
            height: '1.25rem',
            flexShrink: 0,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            borderRadius: theme.vars.shape.radiusSm,
            background: theme.vars.palette.background.button.drawerMenu.selected,
            ...theme.typography.labelSmall,
            fontWeight: 600,
          })}
        >
          {name.charAt(0).toUpperCase()}
        </Box>
        <Typography variant="labelSmall" component="span" noWrap sx={{ flex: 1, minWidth: 0, textAlign: 'start', fontWeight: 600 }}>
          {name}
        </Typography>
        <UnfoldMoreIcon sx={(theme: Theme) => ({ width: '1rem', height: '1rem', color: theme.vars.palette.text.metrics })} />
      </ButtonBase>
      <ProjectPicker
        anchor={anchor}
        projects={projects}
        selectedProjectId={selectedProjectId}
        onClose={() => setAnchor(null)}
        onPick={(project) => {
          setAnchor(null);
          if (String(project.id) !== selectedProjectId) onSelectProject(String(project.id), project.name);
        }}
      />
    </>
  );
}
