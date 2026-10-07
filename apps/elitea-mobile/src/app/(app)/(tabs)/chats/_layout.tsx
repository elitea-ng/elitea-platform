import { router, Stack } from 'expo-router';

import { useDesign } from '@/design/DesignProvider';
import { notificationBellAction, useNotificationCount } from '@/features/notifications/NotificationBell';
import { projectSwitcherOptions, useProjectLabel } from '@/features/projects/ProjectChip';
import { workspaceHeaderOptions } from '@/features/workspaces/workspaceHeader';
import { headerActionOptions, largeTitleOptions } from '@/ui';

/**
 * The Chats tab: a large title (collapsing into the inline title, which names the workspace
 * host), the project switcher in the leading slot, and two icon actions in one glass group:
 * the bell with its badge and New chat (audit 2.8; X5/X6).
 */
export default function ChatsTabLayout() {
  const design = useDesign();
  const unseen = useNotificationCount();
  const project = useProjectLabel();
  return (
    <Stack screenOptions={workspaceHeaderOptions}>
      <Stack.Screen
        name="index"
        options={{
          title: 'Chats',
          ...largeTitleOptions(design),
          ...projectSwitcherOptions(project),
          ...headerActionOptions(
            [
              notificationBellAction(unseen),
              { id: 'new', icon: 'newChat', label: 'New chat', onPress: () => router.push('/new') },
            ],
            design,
          ),
        }}
      />
    </Stack>
  );
}
