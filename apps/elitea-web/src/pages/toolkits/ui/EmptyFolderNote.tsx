import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/** What an open folder with nothing in it says, in place of "create your first toolkit". */
export function EmptyFolderNote(): ReactNode {
  return (
    <Typography
      variant="bodyMedium"
      component="output"
      data-testid="toolkits-empty-folder"
      sx={{ padding: '2rem', textAlign: 'center' }}
    >
      {t('pages.toolkits.toolkits.emptyFolder', 'This folder is empty. Move items into it with the folder icon on a card.')}
    </Typography>
  );
}
