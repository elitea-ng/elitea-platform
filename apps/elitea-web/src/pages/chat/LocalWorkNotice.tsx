/**
 * What a desktop Local work thread shows in place of the composer.
 *
 * The thread's turns ran on someone's computer over a local folder; the
 * server cannot see that folder, so it refuses to continue the thread (409
 * `local_work_thread`) and the chat page is read-only: this notice replaces
 * the composer, and `ChatBox` drops regenerate and edit-and-resend. Reading,
 * export, rename and delete stay where they are.
 *
 * In the desktop build a folder on this computer may have run the thread; the
 * lazily loaded `OpenLocalThread` then offers to open it there. The web build
 * drops that import (the `MODE` check is a build-time literal).
 */
import { lazy, Suspense } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

const OpenLocalThread = import.meta.env.MODE === 'desktop' ? /* @__PURE__ */ lazy(() => import('@/pages/workspace/OpenLocalThread')) : null;

export interface LocalWorkNoticeProps {
  readonly conversationId: string;
  /** The folder's name, when the desktop recorded it on the conversation. */
  readonly folderName: string | undefined;
}

export function LocalWorkNotice({ conversationId, folderName }: LocalWorkNoticeProps): React.JSX.Element {
  const where = folderName === undefined
    ? t('pages.chat.localWork.where', 'This thread works on files on your computer.')
    : t('pages.chat.localWork.whereFolder', 'This thread works on files on your computer, in {{folder}}.', { folder: folderName });
  return (
    <Alert
      severity="info"
      data-testid="local-work-notice"
      action={OpenLocalThread !== null ? <Suspense fallback={null}><OpenLocalThread conversationId={conversationId} /></Suspense> : undefined}
    >
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
        <Typography variant="bodyMedium">{where}</Typography>
        <Typography variant="bodySmall">
          {t('pages.chat.localWork.continue', 'To continue it, open the thread in the Elitea desktop app on that computer. Here you can read it, export it, rename it or delete it.')}
        </Typography>
      </Box>
    </Alert>
  );
}
