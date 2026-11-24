import type { SxProps, Theme } from '@mui/material/styles';

export const onDemandContainerSx = (isSmallWindow: boolean, isOpen: boolean, width: number): SxProps<Theme> => ({
  position: 'relative',
  display: isOpen ? 'flex' : 'none',
  flexDirection: 'row',
  flexShrink: 0,
  width: isSmallWindow ? '100%' : `${width / 16}rem`,
  maxWidth: isSmallWindow ? '100%' : '50%',
  minWidth: isSmallWindow ? '100%' : '20rem',
  height: isSmallWindow ? 'auto' : '100%',
  minHeight: isSmallWindow ? 'calc(100vh - 13.75rem)' : undefined,
  boxSizing: 'border-box',
});

export const legacyContainerSx = (isSmallWindow: boolean, collapsed: boolean, display: string | undefined): SxProps<Theme> => ({
  minWidth: isSmallWindow ? '100%' : collapsed ? '1.75rem' : '20rem',
  width: isSmallWindow ? '100%' : collapsed ? '1.75rem' : '20rem',
  flex: isSmallWindow ? '0 0 auto' : undefined,
  flexShrink: 0,
  maxWidth: '100%',
  position: 'relative',
  height: isSmallWindow ? 'auto' : '100%',
  display: display ?? 'flex',
  flexDirection: 'row',
  boxSizing: 'border-box',
  gap: '0.75rem',
});

export const resizeHandleSx: SxProps<Theme> = (theme: Theme) => ({
  position: 'absolute',
  top: 0,
  bottom: 0,
  left: '-0.75rem',
  width: '0.75rem',
  margin: 0,
  border: 'none',
  zIndex: 2,
  cursor: 'col-resize',
  '&::after': {
    content: '""',
    position: 'absolute',
    top: '50%',
    left: '50%',
    transform: 'translate(-50%, -50%)',
    width: '0.1875rem',
    height: '2rem',
    borderRadius: theme.vars.shape.radiusSm,
    backgroundColor: theme.vars.palette.border.lines,
    transition: 'background-color 0.15s ease',
  },
  '&:hover::after': {
    backgroundColor: theme.vars.palette.primary.main,
  },
});

export const collapseButtonSx: SxProps<Theme> = { padding: '0', marginLeft: '0', position: 'absolute', top: '0.25rem', left: '0', zIndex: 100 };

export const chatContentSx = (isSmallWindow: boolean): SxProps<Theme> => ({
  display: 'flex',
  flexDirection: 'column',
  height: isSmallWindow ? 'auto' : '100%',
  width: '100%',
  maxWidth: '100%',
  flex: 1,
  minHeight: 0,
  overflowY: isSmallWindow ? 'visible' : 'auto',
  gap: '0.75rem',
  boxSizing: 'border-box',
});

export const topBarSx = (isSmallWindow: boolean, hidden: boolean, isOnDemand: boolean): SxProps<Theme> => ({
  display: !hidden ? 'flex' : 'none',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '0.5rem',
  width: '100%',
  minHeight: '1.75rem',
  marginTop: isOnDemand ? '0.25rem' : undefined,
  paddingLeft: isOnDemand || isSmallWindow ? '0' : '2.25rem',
});

export const controlsContainerSx = (isSmallWindow: boolean, hasActiveConversationId: boolean): SxProps<Theme> => ({
  display: 'flex',
  flex: 1,
  alignItems: 'center',
  justifyContent: 'flex-end',
  marginRight: !isSmallWindow && hasActiveConversationId ? '0.25rem' : '0',
  gap: '0.5rem',
});

export const closeDividerSx: SxProps<Theme> = (theme: Theme) => ({
  marginY: '0.25rem',
  borderColor: theme.vars.palette.border.lines,
});

export const closeButtonSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.icon.fill.secondary,
});
