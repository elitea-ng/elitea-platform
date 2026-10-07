import { Redirect, router, Stack, useGlobalSearchParams } from 'expo-router';
import { useState } from 'react';

import { DesignProvider, useDesign } from '@/design/DesignProvider';
import { GALLERY_BRANDS, parseGalleryParams } from '@/features/designGallery/DesignGallery';
import { Button, sheetOptions } from '@/ui';

/**
 * Development builds only: the secondary screens with sample data (`/design-secondary?screen=…`),
 * drawn by the views their real routes render (D2): Discover, the agent sheet, notifications,
 * memory, signed-in devices, About and the full-screen states. `&brand=magenta|green|lemon`
 * (read once, when the preview opens) applies a sample workspace accent; the scheme follows the
 * device appearance.
 */
export default function DesignSecondaryLayout() {
  const params = useGlobalSearchParams<{ brand?: string }>();
  const [brand] = useState(() => (params.brand === undefined ? undefined : GALLERY_BRANDS[parseGalleryParams(params).brand]));
  if (!__DEV__) return <Redirect href="/" />;
  if (brand === undefined) return <PreviewStack />;
  return (
    <DesignProvider brand={brand}>
      <PreviewStack />
    </DesignProvider>
  );
}

function PreviewStack() {
  const design = useDesign();
  return (
    <Stack screenOptions={{ headerTintColor: design.colors.accent, headerTitleStyle: { color: design.colors.textPrimary } }}>
      <Stack.Screen name="index" />
      {/* As the real agent route: a sheet with detents over the catalogue, Done for VoiceOver. */}
      <Stack.Screen
        name="agent"
        options={{
          title: 'Agent',
          ...sheetOptions(design, 'detail'),
          headerRight: () => <Button title="Done" variant="plain" size="small" onPress={() => router.back()} />,
        }}
      />
    </Stack>
  );
}
