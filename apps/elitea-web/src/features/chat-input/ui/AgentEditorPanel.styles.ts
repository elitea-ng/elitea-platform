import type { Theme } from '@mui/material/styles';

/** Style factory for `AgentEditorPanel.tsx`, split out to keep that file under the §3.5 budgets. */
export function agentEditorPanelStyles(isSmallView: boolean) {
  return {
    outerContainer: (theme: Theme) => ({
      display: 'flex',
      alignItems: 'center',
      gap: '0.25rem',
      padding: '0.25rem',
      borderRadius: theme.vars.shape.radiusPill,
      border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
      minWidth: 0,
      maxWidth: '100%',
      // A14 (ELITEA-0386): `ButtonGroup`'s own `overflow: visible` means a
      // flex-shrunk group does not clip its overflowing children — they
      // render PAST the group's own (shrunk) box, visually landing on top
      // of `SwitchToModelButton`, the very next flex sibling (measured live:
      // the group shrank to 114px against ~151px of real button content,
      // and the 6th button rendered at the same x-coordinate
      // `SwitchToModelButton` was placed at). `overflowX: 'auto'` turns that
      // silent visual overlap into a scrollbar instead — happens only when
      // the composer is this narrow (participants rail expanded AND
      // `isSmallView`), which is precisely when this row was already
      // tightest.
      overflowX: 'auto',
    }),
    buttonRow: {
      display: 'flex',
      alignItems: 'center',
      minWidth: 0,
      maxWidth: '100%',
      // Stops the group being flex-shrunk below its buttons' real content
      // width in the first place — see `outerContainer`'s own comment for
      // what shrinking it silently caused.
      flexShrink: 0,
    },
    entityIconWrapper: {
      marginRight: isSmallView ? 0 : '.5rem',
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
    },
    editingText: { fontWeight: 400 },
    settingIcon: { width: '1rem', height: '1rem' },
    closeButton: { padding: '0.375rem', flexShrink: 0 },
    participantName: {
      overflow: 'hidden',
      textOverflow: 'ellipsis',
      whiteSpace: 'nowrap',
      maxWidth: '100%',
    },
  };
}

export type AgentEditorPanelStyles = ReturnType<typeof agentEditorPanelStyles>;
