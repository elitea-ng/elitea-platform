import { Redirect, useLocalSearchParams } from 'expo-router';

import { DesignGallery, parseGalleryParams } from '@/features/designGallery/DesignGallery';

/**
 * Development builds only: the design gallery (src/features/designGallery/). The root stack
 * registers this route behind `Stack.Protected guard={__DEV__}`, and a release build that is
 * sent here anyway goes to the app's start. `?scheme=light|dark&brand=zefir|magenta|green|lemon`
 * preselects the preview (used for screenshots).
 */
export default function DesignGalleryRoute() {
  const params = useLocalSearchParams<{ scheme?: string; brand?: string }>();
  if (!__DEV__) return <Redirect href="/" />;
  const { scheme, brand } = parseGalleryParams(params);
  return <DesignGallery initialScheme={scheme} initialBrand={brand} />;
}
