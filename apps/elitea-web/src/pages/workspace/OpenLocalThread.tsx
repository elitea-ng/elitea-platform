/**
 * The desktop's half of the Local work notice on a chat page: when a folder
 * on this computer ran the thread, a button that opens it there. Renders
 * nothing while looking, and nothing when no folder here matches (the notice
 * alone then says where the thread can be continued).
 *
 * Desktop-only: `pages/chat` loads it lazily behind
 * `import.meta.env.MODE === 'desktop'`.
 */
import { useEffect, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import Button from '@mui/material/Button';

import { useWorkspaceIpc } from '@/features/workspace';
import { t } from '@/shared/i18n';

import { findLocalWorkThread } from './localThreadLookup';

export default function OpenLocalThread({ conversationId }: { conversationId: string }): React.JSX.Element | null {
  const ipc = useWorkspaceIpc();
  const navigate = useNavigate();
  const [workspaceId, setWorkspaceId] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    setWorkspaceId(null);
    void findLocalWorkThread(conversationId, ipc).then((found) => {
      if (live) setWorkspaceId(found);
    });
    return () => {
      live = false;
    };
  }, [conversationId, ipc]);

  if (workspaceId === null) return null;
  return (
    <Button
      variant="elitea"
      color="secondary"
      onClick={() => void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId }, search: { conversation: conversationId } })}
    >
      {t('workspace.localWork.openThread', 'Open in Local work')}
    </Button>
  );
}
