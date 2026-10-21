import { Stack } from 'expo-router';

import { useDesign } from '@/design/DesignProvider';
import { largeTitleOptions } from '@/ui';

export default function DesignSettingsLayout() {
  const design = useDesign();
  return (
    <Stack>
      <Stack.Screen name="index" options={{ title: 'Settings', ...largeTitleOptions(design) }} />
    </Stack>
  );
}
