import { useLocalSearchParams } from 'expo-router';
import { useEffect } from 'react';

import { rememberOpenChat } from '@/features/chats/selectedChat';
import { ThreadView } from '@/features/chat/ThreadView';
import { useSession } from '@/state/session';

/**
 * A conversation on its own screen, above the tabs: from the Chats list at compact width (a
 * phone, iPad Split View at 1/3), Home, New chat, a widget, a quick action, Siri or a link.
 * It becomes the Chats tab's selection, so at regular width (issue #15) the list shows it
 * selected and the detail column shows it once this screen is closed.
 */
export default function ThreadScreen() {
  const { id, intent } = useLocalSearchParams<{ id: string; intent?: string }>();
  const ref = typeof id === 'string' ? id : '';
  const workspaceId = useSession((s) => s.workspaceId);
  const projectId = useSession((s) => s.projectId);
  useEffect(() => {
    rememberOpenChat({ workspaceId, projectId }, ref);
  }, [workspaceId, projectId, ref]);
  return <ThreadView id={ref} intent={intent} />;
}
