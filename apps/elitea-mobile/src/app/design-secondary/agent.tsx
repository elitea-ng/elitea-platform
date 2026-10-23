import { PublicAgentView } from '@/features/discover/PublicAgentView';
import { SAMPLE_PROJECT_ID, SAMPLE_PROJECT_NAME } from '@/features/designGallery/fixtures';
import { SAMPLE_PUBLIC_AGENTS, SAMPLE_STARTERS, SAMPLE_WELCOME } from '@/features/designGallery/secondaryFixtures';

/** Development builds only: the agent sheet with a sample agent (the view `/discover/[id]` renders, D2). */
export default function DesignAgentPreview() {
  return (
    <PublicAgentView
      agent={SAMPLE_PUBLIC_AGENTS[0]}
      projectId={SAMPLE_PROJECT_ID}
      projectLabel={SAMPLE_PROJECT_NAME}
      welcome={SAMPLE_WELCOME}
      starters={SAMPLE_STARTERS}
      busy={null}
      error={null}
      onStart={() => {}}
      onStarter={() => {}}
    />
  );
}
