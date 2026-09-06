/**
 * ProjectContext styles.
 */
import type { Theme } from '@mui/material/styles';

export const projectContextStyles = {
  toggleCard: {
    root: ({ palette }: { palette: Theme['palette'] }) => ({
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'space-between',
      padding: '1rem 1.5rem',
      borderRadius: 'var(--el-shape-radiusMd, 8px)',
      backgroundColor: palette.background.userInputBackground,
      gap: '1rem',
    }),
    text: {
      display: 'flex',
      flexDirection: 'column',
      gap: '0.25rem',
    },
  },
  editor: () => ({
    section: {
      display: 'flex',
      flexDirection: 'column',
      flex: 1,
      minHeight: 0,
      marginTop: '0.5rem',
    },
    header: {
      display: 'flex',
      alignItems: 'flex-start',
      justifyContent: 'space-between',
      gap: '1rem',
      paddingBottom: '0.75rem',
    },
    textBlock: {
      display: 'flex',
      flexDirection: 'column',
      gap: '0.25rem',
    },
    toolbar: {
      display: 'flex',
      alignItems: 'center',
      gap: '1rem',
      flexShrink: 0,
      alignSelf: 'center',
    },
    wrapper: ({ palette }: { palette: Theme['palette'] }) => ({
      display: 'flex',
      flex: 1,
      minHeight: 0,
      borderRadius: 'var(--el-shape-radiusSm, 4px)',
      border: `0.0625rem solid ${palette.border.table}`,
      overflow: 'hidden',
      '& .cm-editor': { backgroundColor: palette.background.codeMirrorEditor },
      '&:focus-within': { borderColor: palette.primary.main },
      '& .cm-theme': { width: '100%' },
      '& .cm-gutters': {
        backgroundColor: 'transparent',
        borderRight: `0.0625rem solid ${palette.border.table}`,
      },
    }),
    preview: ({ palette }: { palette: Theme['palette'] }) => ({
      flex: 1,
      minHeight: 0,
      padding: '0.75rem',
      borderRadius: 'var(--el-shape-radiusSm, 4px)',
      border: `0.0625rem solid ${palette.border.table}`,
      backgroundColor: palette.background.userInputBackground,
      overflow: 'auto',
    }),
  }),
  root: (): Record<string, unknown> => ({
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    alignItems: 'center',
  }),
  loader: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
  },
  body: {
    flex: 1,
    overflow: 'auto',
    minHeight: 0,
    padding: '1rem 1.5rem',
    paddingBottom: '2.375rem',
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    width: '100%',
    // 46.875rem (750px), the reference's own value in every project-context
    // view (`ProjectContextEditor.jsx:334`, `ProjectContextSavedView.jsx:201`)
    // and in `ProjectGeneralContent.jsx`. This app had 43.75rem, so the column
    // was 50px narrower than production on one tab and correct on none.
    maxWidth: '46.875rem',
  },
  charCounterWrapper: {
    display: 'flex',
    justifyContent: 'flex-end',
    paddingTop: '0.25rem',
  },
  actions: {
    display: 'flex',
    gap: '0.75rem',
    paddingLeft: 0,
    paddingTop: '0.25rem',
    marginTop: 'auto',
    width: '100%',
  },
};
