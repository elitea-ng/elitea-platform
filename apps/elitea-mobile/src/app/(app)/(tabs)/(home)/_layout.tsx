import { Stack } from 'expo-router';

import { workspaceHeaderOptions } from '@/features/workspaces/workspaceHeader';

/**
 * The Home tab's stack. The screen sets its own bar (`homeHeaderOptions`): transparent over the
 * gradient, avatar and bell, the inline "Home" once the greeting has scrolled away. The inline
 * title names the workspace host like every signed-in title (`workspaceHeaderOptions`); the
 * expanded state shows the host in the workspace pill.
 */
export default function HomeTabLayout() {
  return (
    <Stack screenOptions={workspaceHeaderOptions}>
      <Stack.Screen name="index" options={{ title: 'Home' }} />
    </Stack>
  );
}
