import { Stack } from 'expo-router';

import { useDesign } from '@/design/DesignProvider';
import { largeTitleOptions } from '@/ui';

export default function DesignDiscoverLayout() {
  const design = useDesign();
  return (
    <Stack>
      <Stack.Screen name="index" options={{ title: 'Discover', ...largeTitleOptions(design) }} />
    </Stack>
  );
}
