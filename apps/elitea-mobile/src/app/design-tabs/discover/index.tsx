import { router, Stack } from 'expo-router';
import { useState } from 'react';

import { useDesign } from '@/design/DesignProvider';
import { DiscoverView, type DiscoverLayout } from '@/features/discover/DiscoverView';
import { SAMPLE_ORIGIN, SAMPLE_PROJECT_ID } from '@/features/designGallery/fixtures';
import { SAMPLE_PUBLIC_AGENTS } from '@/features/designGallery/secondaryFixtures';
import { WorkspacePillView } from '@/features/workspaces/WorkspacePill';
import { headerActionOptions, searchBarOptions } from '@/ui';

/**
 * Development builds only: the Discover tab with sample agents (`/design-tabs/discover`), drawn by
 * the real `DiscoverView` (D2) under the same native search field and grid/list bar button. A card
 * opens the agent sheet preview.
 */
export default function DesignDiscoverPreview() {
  const design = useDesign();
  const [layout, setLayout] = useState<DiscoverLayout>('grid');
  return (
    <>
      <Stack.Screen
        options={{
          ...searchBarOptions(design, { placeholder: 'Search public agents', onChange: () => {} }),
          ...headerActionOptions(
            [
              layout === 'grid'
                ? { id: 'layout', icon: 'list', label: 'Show as list', onPress: () => setLayout('list') }
                : { id: 'layout', icon: 'grid', label: 'Show as grid', onPress: () => setLayout('grid') },
            ],
            design,
          ),
        }}
      />
      <DiscoverView
        agents={SAMPLE_PUBLIC_AGENTS}
        projectId={SAMPLE_PROJECT_ID}
        layout={layout}
        status="ready"
        query=""
        onOpen={() => router.push('/design-secondary/agent')}
        onRetry={() => {}}
        header={<WorkspacePillView origin={SAMPLE_ORIGIN} image={null} />}
      />
    </>
  );
}
