import { Redirect, router, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';

import { DesignProvider } from '@/design/DesignProvider';
import { GALLERY_BRANDS, parseGalleryParams } from '@/features/designGallery/DesignGallery';
import { SAMPLE_ORIGIN } from '@/features/designGallery/fixtures';
import { ProjectsSheet } from '@/features/projects/ProjectsSheet';

/**
 * Development builds only: the project switcher sheet with sample projects, rendering the real
 * `ProjectsSheet` (D2): the workspace on top, then the projects by name.
 */
export default function DesignProjectsRoute() {
  const params = useLocalSearchParams<{ brand?: string }>();
  const [brand] = useState(() => GALLERY_BRANDS[parseGalleryParams(params).brand]);
  const [selected, setSelected] = useState(90504);
  if (!__DEV__) return <Redirect href="/" />;
  const sheet = (
    <ProjectsSheet
      workspace={{ origin: SAMPLE_ORIGIN, name: brand ? 'Acme Corp AI' : null, logoMark: null, brand }}
      loading={false}
      failed={false}
      onRetry={() => {}}
      personal={{ id: 90501, name: 'project_user_8', suspended: false }}
      team={[
        { id: 90504, name: 'Zefir Team', suspended: false },
        { id: 90510, name: 'Marketing', suspended: false },
        { id: 90511, name: 'Customer Success EMEA', suspended: false },
      ]}
      selectedId={selected}
      onSelect={setSelected}
      onSwitchWorkspace={() => router.replace('/design-workspaces')}
    />
  );
  // A sample brand (`?brand=`) previews the sheet as that workspace's accent draws it.
  return params.brand === undefined ? sheet : <DesignProvider brand={brand}>{sheet}</DesignProvider>;
}
