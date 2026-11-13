import { useState, type ReactNode } from 'react';

import { GenerateAgentModal } from '@/features/agents';
import { hasBackendCapability } from '@/shared/config';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { AiSparkleIcon } from '@/shared/ui/icons/ai-sparkle-icon';

/**
 * "Build with AI" on the new-agent page (#2022): describe the agent in one
 * prompt and the platform drafts it, then opens the agent it created.
 *
 * The modal and its draft pipeline were already built and mounted from the
 * workspace's create-agent dialog, but the create page's
 * `generateAgentButtonSlot` was never filled, so the option the old product
 * offered on this screen was missing. Hidden when the deployment does not
 * serve the draft endpoint, as the modal's own button is.
 */
export interface BuildWithAiButtonProps {
  readonly projectId: string | undefined;
  /** Called with the id of the agent the draft created. */
  readonly onCreated: (agentId: string) => void;
}

export function BuildWithAiButton({ projectId, onCreated }: BuildWithAiButtonProps): ReactNode {
  const [open, setOpen] = useState(false);
  if (!hasBackendCapability('aiGeneration')) return null;
  return (
    <>
      <BaseBtn
        variant="secondary"
        size="small"
        startIcon={<AiSparkleIcon />}
        data-testid="build-with-ai-button"
        onClick={() => setOpen(true)}
      >
        {t('features.agents.generateAgentButton.label', 'Build with AI')}
      </BaseBtn>
      <GenerateAgentModal
        open={open}
        onClose={() => setOpen(false)}
        projectId={projectId}
        onAgentCreated={(result) => onCreated(String(result.id))}
      />
    </>
  );
}
