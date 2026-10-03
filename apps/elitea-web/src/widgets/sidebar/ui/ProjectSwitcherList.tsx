import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { Project } from '@/entities/project';
import { CheckedIcon } from '@/shared/ui/icons/checked-icon';

import { useScrollFades } from '../lib/useScrollFades';

import { ProjectAvatar } from './ProjectAvatar';

export interface ProjectSwitcherListProps {
  readonly projects: readonly Project[];
  readonly selectedProjectId: string | undefined;
  readonly noneLabel: string;
  /** The popup's open state. The list re-measures its fades each time it opens. */
  readonly open: boolean;
  readonly onSelect: (project: Project) => void;
}

/**
 * The option rows of `ProjectSwitcher`'s popup. Split out of
 * `ProjectSwitcher.tsx` for the file-length and complexity budgets.
 *
 * #6712: the project rows scroll inside their own box, and a fade at the top
 * or bottom edge shows that more projects exist in that direction. Each fade
 * shows only while content is hidden on its side. The "request a project"
 * footer stays outside this box, so it never scrolls away.
 */
export function ProjectSwitcherList({ projects, selectedProjectId, noneLabel, open, onSelect }: ProjectSwitcherListProps): ReactNode {
  const listScroll = useScrollFades(open ? projects.length : -1);

  return (
    <Box sx={{ position: 'relative' }}>
      <Box
        ref={listScroll.ref}
        onScroll={listScroll.onScroll}
        data-testid="project-switcher-list"
        sx={{ maxHeight: '16rem', overflowY: 'auto' }}
      >
        {projects.length === 0 && (
          <Box
            // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- same custom listbox as the option rows below; an empty row still has to be an "option" for the listbox to stay valid ARIA, exactly as MUI renders a disabled `MenuItem` inside `Select`.
            role="option"
            aria-selected={false}
            aria-disabled
            sx={(theme: Theme) => ({
              display: 'flex',
              alignItems: 'center',
              padding: theme.spacing(1, 2),
              cursor: 'default',
              color: theme.vars.palette.text.metrics,
            })}
          >
            <Typography variant="labelMedium">{noneLabel}</Typography>
          </Box>
        )}
        {projects.map((project) => (
          <Box
            key={project.id}
            // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- no native tag maps to an ARIA "option" outside a real <select>/<datalist>; this is a custom-styled listbox (role="listbox" on the Paper above), the standard pattern for exactly this case.
            role="option"
            aria-selected={String(project.id) === selectedProjectId}
            tabIndex={0}
            onClick={() => onSelect(project)}
            onKeyDown={(event) => {
              if (event.key === 'Enter' || event.key === ' ') {
                event.preventDefault();
                onSelect(project);
              }
            }}
            sx={(theme: Theme) => ({
              display: 'flex',
              alignItems: 'center',
              // 0.5rem, matching the trigger above. The row used 0.75rem,
              // so the avatar column stepped sideways between the closed
              // control and the open list — most visible now that the
              // panel is the same width as the rail.
              gap: '0.5rem',
              padding: theme.spacing(1, 2),
              cursor: 'pointer',
              '&:hover': { backgroundColor: theme.vars.palette.action.hover },
            })}
          >
            <ProjectAvatar
              projectName={project.name}
              size="1.5rem"
            />
            <Typography
              variant="labelMedium"
              sx={{
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
                flex: 1,
                minWidth: 0,
              }}
            >
              {project.name}
            </Typography>
            {String(project.id) === selectedProjectId && (
              // The selected row carried aria-selected and NOTHING a
              // sighted user could see. `SingleSelectMenuItem` — the
              // shared dropdown row this control deliberately does not
              // use, because it cannot host an avatar — marks selection
              // with this same icon, so the affordance matches the rest
              // of the app rather than inventing a third convention.
              <CheckedIcon
                aria-hidden
                data-testid="project-switcher-selected"
                style={{ flexShrink: 0 }}
              />
            )}
          </Box>
        ))}
      </Box>
      {listScroll.fades.showTop && (
        <Box
          aria-hidden
          data-testid="project-switcher-fade-top"
          sx={(theme: Theme) => ({
            ...fadeSx,
            top: 0,
            background: `linear-gradient(to bottom, ${theme.vars.palette.background.secondary}, transparent)`,
          })}
        />
      )}
      {listScroll.fades.showBottom && (
        <Box
          aria-hidden
          data-testid="project-switcher-fade-bottom"
          sx={(theme: Theme) => ({
            ...fadeSx,
            bottom: 0,
            background: `linear-gradient(to top, ${theme.vars.palette.background.secondary}, transparent)`,
          })}
        />
      )}
    </Box>
  );
}

/** The shared geometry of the two scroll fades: a 24px band that lets clicks pass through. */
const fadeSx = {
  position: 'absolute',
  left: 0,
  right: 0,
  height: '1.5rem',
  pointerEvents: 'none',
} as const;
