import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';

import { hasBackendCapability } from '@/shared/config';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { AiSparkleIcon } from '@/shared/ui/icons/ai-sparkle-icon';

import { useSelectedProjectId } from '../../api/useSelectedProjectId';
import { useHasPermission } from '../../lib/useHasPermission';
import { GenerateAgentModal, type GenerateAgentModalProps } from './GenerateAgentModal';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentButton.jsx`,
 * merged with `entities/generate-entity-with-ai/ui/GenerateEntityButton.jsx`'s
 * permission-gated open/close chrome — see `GenerateAgentModal.tsx`'s own
 * doc comment for why the generic `entities/generate-entity-with-ai` layer
 * is inlined here rather than duplicated as a second unused-elsewhere
 * abstraction.
 *
 * `useModal()` (baseline: `shared/lib/hooks/useModal.hooks`) has no port —
 * its entire behaviour is a boolean `useState`, reproduced inline.
 * `useCheckPermission` (baseline) is replaced by this slice's own
 * `useHasPermission` — see that file's doc comment for the layering
 * reason (`widgets/sidebar/api/usePermissionSet.ts` is upward-import-illegal
 * from `features/`).
 *
 * This button is only ever rendered inside `CreateAgentForm`'s
 * `generateAgentButtonSlot`, which `BasicAccordion` takes as its
 * `summaryAction`. `BaseBtn` is passed `component="span"`, which makes the
 * underlying MUI `ButtonBase` resolve `nativeButton` to `false` and render
 * `role="button"` with its own synthesized Enter/Space keyboard activation
 * instead of a `<button>` tag.
 *
 * THAT IS NO LONGER A CONSTRAINT, only what this file happens to do.
 * `summaryAction` used to render INSIDE `StyledAccordionSummary`'s own
 * `<button>`, where a nested `<button>` is invalid HTML — and where the
 * `role="button"` span was no better to axe, which fails any focusable
 * descendant (`nested-interactive`, J18). `BasicAccordion` now renders the
 * action beside that button, so a real `<button>` is admissible here.
 */
export interface GenerateAgentButtonProps {
  readonly onAgentCreated: GenerateAgentModalProps['onAgentCreated'];
  readonly onApproveError?: GenerateAgentModalProps['onApproveError'];
  readonly onAssociationWarning?: GenerateAgentModalProps['onAssociationWarning'];
}

export function GenerateAgentButton({
  onAgentCreated,
  onApproveError,
  onAssociationWarning,
}: GenerateAgentButtonProps): ReactNode {
  const projectId = useSelectedProjectId();
  const hasPermission = useHasPermission(projectId, PERMISSIONS.applications.update);
  const [isOpen, setIsOpen] = useState(false);

  const handleOpen = useCallback(() => setIsOpen(true), []);
  const handleClose = useCallback(() => setIsOpen(false), []);

  // The draft endpoint is not mounted. See `shared/config/backendCapabilities`.
  if (!hasBackendCapability('aiGeneration')) return null;
  if (!hasPermission) return null;

  return (
    <>
      <BaseBtn
        component="span"
        variant="secondary"
        size="small"
        startIcon={<AiSparkleIcon />}
        onClick={handleOpen}
        sx={buttonSx}
      >
        {t('features.agents.generateAgentButton.label', 'Build with AI')}
      </BaseBtn>
      <GenerateAgentModal
        open={isOpen}
        onClose={handleClose}
        projectId={projectId}
        onAgentCreated={onAgentCreated}
        onApproveError={onApproveError}
        onAssociationWarning={onAssociationWarning}
      />
    </>
  );
}

const buttonSx: SxProps<Theme> = (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusPill,
  color: theme.vars.palette.primary.main,
});
