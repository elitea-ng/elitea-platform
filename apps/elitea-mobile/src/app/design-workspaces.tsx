import { Redirect, router } from 'expo-router';

import { GALLERY_BRANDS } from '@/features/designGallery/DesignGallery';
import { SAMPLE_ORIGIN } from '@/features/designGallery/fixtures';
import { WorkspacesSheet } from '@/features/workspaces/WorkspacesSheet';
import type { WorkspaceSummary } from '@/state/workspaceList';

/**
 * Development builds only: the workspace switcher sheet with three sample workspaces, each in its
 * own accent (magenta, green, unbranded), rendering the real `WorkspacesSheet` (D2).
 */
const SAMPLE_WORKSPACES: readonly WorkspaceSummary[] = [
  { id: 'aaaaaaaaaaaa', origin: SAMPLE_ORIGIN, name: 'Acme Corp AI', logoMark: null, brand: GALLERY_BRANDS.magenta, signedIn: true, requiresLock: false },
  { id: 'bbbbbbbbbbbb', origin: 'https://zefir.elitea.ai', name: 'Zefir Two', logoMark: null, brand: GALLERY_BRANDS.green, signedIn: true, requiresLock: true },
  { id: 'cccccccccccc', origin: 'https://ai.example.org', name: null, logoMark: null, brand: null, signedIn: false, requiresLock: false },
];

export default function DesignWorkspacesRoute() {
  if (!__DEV__) return <Redirect href="/" />;
  return (
    <WorkspacesSheet
      workspaces={SAMPLE_WORKSPACES}
      activeId="aaaaaaaaaaaa"
      onOpen={() => router.back()}
      onRemove={async () => false}
      onAdd={() => router.back()}
    />
  );
}
