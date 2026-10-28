import type { SxProps, Theme } from '@mui/material/styles';

/**
 * Style objects for `AIAssistantModal`/`AIAssistantModalSplitView`, split
 * out of the component file(s) — module-scope `sx` constants, per §3.6
 * ("Styles objects are module-scope constants ... never built inside the
 * component body"). Ported from baseline `AIAssistantModal.jsx`'s own
 * `aiAssistantModalStyles` (lines 450-580), with every callback rewritten
 * from `({palette}) => ({...palette.x})` to `(theme) => ({...theme.vars.
 * palette.x})` (R-T7).
 */

export const contentWrapperSx: SxProps<Theme> = {
  flex: 1,
  display: 'flex',
  flexDirection: 'column',
  minHeight: 0,
  height: '100%',
  overflowY: 'auto',
};

export function contentBackgroundSx(showSplitView: boolean): SxProps<Theme> {
  return (theme) =>
    showSplitView
      ? { background: theme.vars.palette.background.secondary }
      : { background: theme.vars.palette.background.default };
}

export const splitViewContainerSx: SxProps<Theme> = {
  flex: 1,
  display: 'flex',
  minHeight: 0,
};

export const panelContainerSx: SxProps<Theme> = {
  flex: 1,
  display: 'flex',
  flexDirection: 'column',
  minWidth: 0,
  overflow: 'hidden',
};

export const improvedPanelContainerSx: SxProps<Theme> = (theme) => ({
  flex: 1,
  display: 'flex',
  flexDirection: 'column',
  minWidth: 0,
  borderLeft: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  overflow: 'hidden',
  backgroundColor: theme.vars.palette.background.card.hover,
});

export const iconButtonSx: SxProps<Theme> = (theme) => ({
  padding: theme.spacing(0.5),
});

export const buttonWrapperSx: SxProps<Theme> = { display: 'inline-block' };

export const editorContainerSx: SxProps<Theme> = {
  flex: 1,
  minHeight: 0,
};

const cmChromeSx = (theme: Theme, background: string): Record<string, unknown> => ({
  height: '100%',
  backgroundColor: background,
  position: 'relative',
  '& .cm-editor': { backgroundColor: background },
  '& .cm-scroller': { backgroundColor: background },
  '& .cm-content': { paddingTop: theme.spacing(1), paddingBottom: theme.spacing(2) },
  '& .cm-gutters': { backgroundColor: background, borderRight: `0.0313rem solid ${theme.vars.palette.border.lines}` },
  '& .cm-lineNumbers .cm-gutterElement': { paddingTop: theme.spacing(0.1) },
  '& .cm-lint-marker': { position: 'relative', top: '0.25rem' },
});

export const currentEditorWrapperSx: SxProps<Theme> = (theme) => cmChromeSx(theme, theme.vars.palette.background.secondary);

export const improvedEditorContainerSx: SxProps<Theme> = (theme) => ({
  flex: 1,
  minHeight: 0,
  position: 'relative',
  backgroundColor: theme.vars.palette.background.card.hover,
});

export const improvedEditorWrapperSx: SxProps<Theme> = (theme) => cmChromeSx(theme, theme.vars.palette.background.card.hover);

export const singleViewContainerSx: SxProps<Theme> = (theme) => ({
  flex: 1,
  minHeight: 0,
  position: 'relative',
  '& .cm-theme': { height: '100%' },
  '& .cm-content': { paddingTop: theme.spacing(1), paddingBottom: theme.spacing(2) },
  '& .cm-lineNumbers .cm-gutterElement': { paddingTop: theme.spacing(0.1) },
  '& .cm-lint-marker': { position: 'relative', top: '0.25rem' },
});

export const singleViewLoadingContainerSx: SxProps<Theme> = (theme) => ({
  position: 'absolute',
  top: theme.spacing(1),
  left: theme.spacing(6.25),
  zIndex: 10,
});

/**
 * NOT in the baseline — the baseline surfaces generation failures via
 * `useToast().toastError(...)`, which has no equivalent here (see this
 * unit's `useAIContentGenerationStreaming.ts` doc comment, deviation 3).
 * This inline banner is the "caller decides how to surface it" half of
 * that deviation, applied inside `AIAssistantModal` itself since it is the
 * one component with a concrete `errorMessage` to show.
 */
export const errorBannerSx: SxProps<Theme> = (theme) => ({
  paddingInline: theme.spacing(3),
  paddingBlock: theme.spacing(1),
  color: theme.vars.palette.status.rejectedText,
});
