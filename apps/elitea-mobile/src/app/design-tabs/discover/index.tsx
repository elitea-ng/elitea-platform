import { ScrollView } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { SAMPLE_ORIGIN } from '@/features/designGallery/fixtures';
import { WorkspacePillView } from '@/features/workspaces/WorkspacePill';
import { EmptyState } from '@/ui';

/**
 * Development builds only: a stand-in for the Discover tab (`/design-tabs/discover`). It exists so
 * the preview's tab bar has the same four tabs as the real one (EH-9: a three-tab bar is narrower,
 * so overlap checks against Home and Chats were made against a bar users never get). The real
 * catalogue needs a server; this screen only holds the tab's place.
 */
export default function DesignDiscoverPreview() {
  const design = useDesign();
  return (
    <ScrollView contentInsetAdjustmentBehavior="automatic" style={{ backgroundColor: design.colors.background }}>
      <WorkspacePillView origin={SAMPLE_ORIGIN} image={null} />
      <EmptyState mascot="bust" title="Discover" body="The workspace's public agents appear here." />
    </ScrollView>
  );
}
