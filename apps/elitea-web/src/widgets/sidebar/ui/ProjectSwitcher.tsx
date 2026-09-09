import type { ReactNode } from 'react';
import { useCallback, useId, useRef, useState } from 'react';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import Paper from '@mui/material/Paper';
import Popper from '@mui/material/Popper';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { Project } from '@/entities/project';
import { RequestProjectDialog } from '@/features/project-requests';
import { t } from '@/shared/i18n';

import { CheckedIcon } from '@/shared/ui/icons/checked-icon';

import { ProjectAvatar } from './ProjectAvatar';

export interface ProjectSwitcherProps {
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelect: (projectId: string, projectName: string) => void;
  collapsed?: boolean;
}

/**
 * Ported from `[fsd]/widgets/sidebar-root/ui/SidebarProjectSelect.jsx` +
 * `components/ProjectSelect.jsx`, on a hand-rolled dropdown (not
 * `shared/ui`'s `SingleSelect`, whose own doc comment records that avatar/
 * custom-renderer support was explicitly dropped from its S1 scope) so the
 * per-project colour avatar survives the port. Reduced-scope notes (no
 * uploaded project-icon image, no "private project" second pin, no
 * `AlertDialog` confirmation before switching mid-edit) are in
 * `../lib/projectOptions.ts` and `../index.ts`.
 *
 * R4: the trigger is wrapped in a `Tooltip` showing the selected project's
 * name (or the same "No projects" fallback the expanded text block already
 * uses) exactly when `collapsed`, matching `customRenderProject`
 * (`SidebarProjectSelect.jsx:25-63`)'s `StyledTooltip` — otherwise a
 * collapsed sidebar leaves no way to tell which project is active without
 * expanding it.
 *
 * Issue #238: the hand-rolled trigger dropped the two things the baseline
 * `Select` supplied for free — a visible dropdown arrow and the
 * `aria-haspopup`/`aria-expanded` pair — so the control read as static text
 * ("Project: Default Project") and nobody knew it could be clicked. Both
 * are restored here:
 *
 * - The arrow is `ExpandMoreIcon`, the same chevron the sibling sidebar
 *   dropdown (`widgets/create-button/ui/CreateEntityButton.tsx`, an
 *   identical `ClickAwayListener` + `Popper` + `Paper` shape) already
 *   rotates 180° on open, and the same icon `shared/ui/SingleSelect` hands
 *   to MUI `Select` as its `IconComponent`. It is `aria-hidden`
 *   (decoration; the ARIA state below carries the meaning) and it is not
 *   rendered when `collapsed`, where the avatar is the whole control.
 * - The trigger owns `aria-haspopup="listbox"`, a live `aria-expanded`, and
 *   `aria-controls` pointing at the popup while it is open; the popup
 *   `Paper` keeps its `role="listbox"` and is named by the trigger. Same
 *   trigger/menu id pairing `shared/ui/ControlsDropdown` uses.
 *
 * The popup no longer hides itself when `projects` is empty (it was gated
 * on `open && projects.length > 0`, which made the button a silent no-op).
 * It now renders one disabled "No projects" row — the same key the trigger
 * text falls back to, and the same in-popup empty row
 * `shared/ui/SingleSelect` renders for an empty option list. Disabling the
 * trigger instead would suppress the R4 collapsed tooltip, because MUI
 * `Tooltip` gets no pointer events from a disabled button.
 */
export function ProjectSwitcher({ projects, selectedProjectId, onSelect, collapsed = false }: ProjectSwitcherProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [requestProjectOpen, setRequestProjectOpen] = useState(false);
  const anchorRef = useRef<HTMLElement | null>(null);
  const generatedId = useId();
  const triggerId = `project-switcher-trigger-${generatedId}`;
  const listboxId = `project-switcher-listbox-${generatedId}`;
  const selected = projects.find((project) => String(project.id) === selectedProjectId);
  const noneLabel = t('widgets.sidebar.projectSwitcher.none', 'No projects');
  const selectedName = selected?.name ?? noneLabel;

  /**
   * The trigger's rendered width, read when the panel opens.
   *
   * Measured rather than derived from SIDE_BAR_WIDTH_PX so the panel tracks
   * whatever the rail actually is — the two widths (208px expanded, 72px
   * collapsed) live in Sidebar.tsx and a third copy here would be a constant
   * to keep in sync by hand.
   */
  const [anchorWidth, setAnchorWidth] = useState<number | undefined>(undefined);

  const close = useCallback(() => setOpen(false), []);
  const toggle = useCallback(() => {
    setAnchorWidth(anchorRef.current?.getBoundingClientRect().width);
    setOpen((value) => !value);
  }, []);
  const handleSelect = useCallback(
    (project: Project) => {
      onSelect(String(project.id), project.name);
      setOpen(false);
    },
    [onSelect],
  );

  return (
    <>
    <ClickAwayListener onClickAway={close}>
      <Box sx={{ position: 'relative' }}>
        <Tooltip
          title={collapsed ? selectedName : ''}
          placement="right"
          enterDelay={500}
          enterNextDelay={500}
        >
          <Box
            component="button"
            type="button"
            id={triggerId}
            ref={anchorRef}
            aria-haspopup="listbox"
            aria-expanded={open}
            aria-controls={open ? listboxId : undefined}
            onClick={toggle}
            sx={(theme: Theme) => ({
              display: 'flex',
              alignItems: 'center',
              justifyContent: collapsed ? 'center' : 'flex-start',
              gap: '0.5rem',
              padding: '0.5rem 1rem',
              cursor: 'pointer',
              minHeight: '3.5rem',
              width: '100%',
              border: 'none',
              background: 'transparent',
              textAlign: 'left',
              boxSizing: 'border-box',
              '&:hover': { backgroundColor: theme.vars.palette.background.button.drawerMenu.hover },
            })}
          >
            <ProjectAvatar
              projectName={selected?.name}
              size="1.5rem"
            />
            {!collapsed && (
              <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0, flex: 1 }}>
                <Typography
                  variant="labelSmall"
                  sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}
                >
                  {t('widgets.sidebar.projectSwitcher.label', 'Project:')}
                </Typography>
                <Typography
                  variant="labelSmall"
                  sx={{
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                    maxWidth: '7.5rem',
                  }}
                >
                  {selectedName}
                </Typography>
              </Box>
            )}
            {!collapsed && (
              <ExpandMoreIcon
                aria-hidden
                data-testid="project-switcher-chevron"
                sx={{
                  width: '1rem',
                  height: '1rem',
                  flexShrink: 0,
                  // currentColor, not a pinned palette token. Every other icon
                  // on this rail inherits — the nav items, both collapse
                  // chevrons in Sidebar.tsx, and the create-button chevron —
                  // and pinning one made this the only sidebar icon whose
                  // colour is an independent input. It rendered in the dark
                  // scheme and vanished in the light one, measured in the
                  // visual suite: the dark baseline carries the glyph and the
                  // light baseline of the same row is flat background. A glyph
                  // painted in the background colour and a glyph that never
                  // painted are the same picture, so inheriting removes the
                  // input rather than guessing which of the two it was.
                  transition: 'transform 0.2s ease-in-out',
                  transform: open ? 'rotate(180deg)' : 'rotate(0deg)',
                }}
              />
            )}
          </Box>
        </Tooltip>

        <Popper
          open={open}
          anchorEl={anchorRef.current}
          placement="bottom-start"
          // Keep the panel inside the viewport and off the trigger. Without
          // these the panel is placed at the anchor's bottom edge with no
          // gap and no flip, so a rail near the bottom of a short window
          // renders its list off-screen.
          modifiers={[
            { name: 'offset', options: { offset: [0, 4] } },
            { name: 'preventOverflow', options: { padding: 8 } },
            { name: 'flip', enabled: true },
          ]}
          sx={{ zIndex: 1300 }}
        >
          <Paper
            id={listboxId}
            aria-labelledby={triggerId}
            // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a real <select>/<datalist> cannot host this custom-styled, avatar-decorated option list; role="listbox" + child role="option" is the standard ARIA pattern for exactly this case.
            role="listbox"
            sx={(theme: Theme) => ({
              background: theme.vars.palette.background.secondary,
              borderRadius: theme.vars.shape.radiusMd,
              border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
              padding: '0.5rem 0',
              // Match the rail, do not exceed it.
              //
              // This was minWidth: '14rem' — 224px against a 208px rail, so
              // the panel always hung over the content beside the sidebar, and
              // over nothing at all when the rail was collapsed to 72px. The
              // width now follows the trigger, which is the rail's width in
              // both states, with a floor so the collapsed rail still gets a
              // readable list rather than a 72px column of clipped names.
              width: anchorWidth,
              minWidth: '11.5rem',
              maxWidth: '18rem',
              maxHeight: '20rem',
              overflowY: 'auto',
              boxShadow: theme.vars.palette.boxShadow.default,
            })}
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
                onClick={() => handleSelect(project)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter' || event.key === ' ') {
                    event.preventDefault();
                    handleSelect(project);
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
            {/*
              Self-service "request a project" (#871). A footer row rather
              than a fourth top-level sidebar entry: every existing project
              in this list was admin-created, and this is the one place a
              member who cannot create one themselves is already looking at
              "which project" — the natural spot to ask for a new one.
            */}
            <Box
              component="hr"
              sx={(theme: Theme) => ({
                border: 'none',
                borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
                margin: '0.25rem 0',
              })}
            />
            <Box
              component="button"
              type="button"
              onClick={() => {
                setOpen(false);
                setRequestProjectOpen(true);
              }}
              data-testid="project-switcher-request-project"
              sx={(theme: Theme) => ({
                display: 'flex',
                alignItems: 'center',
                gap: '0.5rem',
                width: '100%',
                padding: theme.spacing(1, 2),
                cursor: 'pointer',
                border: 'none',
                background: 'transparent',
                textAlign: 'left',
                color: theme.vars.palette.text.link,
                '&:hover': { backgroundColor: theme.vars.palette.action.hover },
              })}
            >
              <Typography variant="labelMedium" color="inherit">
                {t('widgets.sidebar.projectSwitcher.requestProject', '+ Request a project')}
              </Typography>
            </Box>
          </Paper>
        </Popper>
      </Box>
    </ClickAwayListener>
    {/*
      Mounted only once opened, not `open={requestProjectOpen}` on an
      always-rendered element: `RequestProjectDialog` calls `useQuery`/
      `useMutation`, which — unlike the `Dialog` it renders through, which
      skips its OWN children when closed — run on every render regardless of
      an `open` prop, so a caller with no `QueryClientProvider` above it
      (every existing `ProjectSwitcher` unit test) would throw the moment
      this component rendered at all, never mind whether the dialog was
      visible.
    */}
    {requestProjectOpen && (
      <RequestProjectDialog
        open={requestProjectOpen}
        onClose={() => setRequestProjectOpen(false)}
      />
    )}
    </>
  );
}
