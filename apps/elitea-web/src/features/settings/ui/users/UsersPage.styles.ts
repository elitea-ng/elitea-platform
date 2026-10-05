/**
 * UsersPage styles — extracted to keep the component ≤ 400 lines (spec §3.5).
 */
import type { CSSProperties } from 'react';

export const usersPageStyles = {
  container: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column' as const,
    overflow: 'hidden',
    gap: '0.75rem',
    // The 1.5rem gutter DrawerPageHeader gives every other settings page, so
    // the title, table and toolbar stop sitting flush against the settings
    // drawer's divider (and the Invite button against the window edge).
    padding: '0 1.5rem',
  },
  header: {
    display: 'flex',
    flexDirection: 'row' as const,
    justifyContent: 'space-between',
    alignItems: 'center',
    padding: '0.5rem 0',
    gap: '1rem',
    flexWrap: 'wrap' as const,
  },
  title: {
    margin: 0,
  },
  toolbar: {
    display: 'flex',
    flexDirection: 'row' as const,
    alignItems: 'center',
    gap: '0.75rem',
    flexWrap: 'wrap' as const,
  },
  batchActions: {
    display: 'flex',
    flexDirection: 'row' as const,
    alignItems: 'center',
    gap: '0.25rem',
  },
  actionButton: {
    display: 'flex',
    flexDirection: 'row' as const,
    alignItems: 'center',
  },
  tableContainer: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column' as const,
    overflow: 'hidden',
    minHeight: 0,
  },
  pagination: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    padding: '0.5rem 1rem',
  },
  pageSizeSelectContainer: {
    display: 'flex',
    flexDirection: 'row' as const,
    alignItems: 'center',
  },
  pageSizeSelect: {
    padding: '0.25rem 0.5rem',
    border: '1px solid',
    borderColor: 'divider',
    borderRadius: 'var(--el-shape-radiusSm, 4px)',
    backgroundColor: 'background.paper',
  } as CSSProperties,
  paginationButtons: {
    display: 'flex',
    flexDirection: 'row' as const,
    alignItems: 'center',
    gap: '0.25rem',
    marginLeft: '0.75rem',
  },
} as const;
