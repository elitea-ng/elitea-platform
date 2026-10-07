import { Stack } from 'expo-router';

import { useDesign } from '@/design/DesignProvider';
import { workspaceHeaderOptions } from '@/features/workspaces/workspaceHeader';
import { largeTitleOptions } from '@/ui';

export default function SettingsTabLayout() {
  const design = useDesign();
  return (
    <Stack screenOptions={workspaceHeaderOptions}>
      <Stack.Screen name="index" options={{ title: 'Settings', ...largeTitleOptions(design) }} />
    </Stack>
  );
}
