/**
 * GenerateProjectContextButton — permission-gated button that opens the
 * GenerateProjectContextModal.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/project-context/GenerateProjectContextButton.jsx`,
 * merged with the step-machine pattern from
 * `features/agents/ui/generate-agent-modal/GenerateAgentButton.tsx`.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';

import { hasBackendCapability } from '@/shared/config';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { AiSparkleIcon } from '@/shared/ui/icons/ai-sparkle-icon';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';

import { GenerateProjectContextModal } from './GenerateProjectContextModal';

export interface GenerateProjectContextButtonProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId: string;
  /** Existing context content. */
  existingContent: string;
  /** Called with the generated content on approve. */
  onApply: (content: string) => void;
  /**
   * Open the dialog as soon as this becomes true, without a click.
   *
   * The empty state's "Build with AI" is the reference's
   * `onNavigate('create', { openAi: true })` (`ProjectContextEmptyState.jsx`),
   * which routes to the editor with `state.openAi` set and the editor opens
   * the generate dialog off that. This app has no separate editor route, so
   * the same intent travels as a prop instead of as router state.
   */
  openAiOnMount?: boolean;
}

/**
 * Own, independent permission gate — old-app parity:
 * `entities/generate-entity-with-ai/ui/GenerateEntityButton.jsx:10-15`
 * (`if (!checkPermission(permission)) return null;`, called with
 * `permission={PERMISSIONS.projectContext.edit}`). Deliberately does not
 * rely solely on the parent only mounting this component when it thinks
 * edit is allowed — this button must hide itself even if a future caller
 * gets that wiring wrong.
 *
 * Inlined rather than imported from `widgets/sidebar`'s `usePermissionSet`:
 * `features/` sits below `widgets/` in the layer order (spec §3.2), so that
 * import would be upward-illegal — same reasoning as
 * `features/agents/lib/useHasPermission.ts`'s own doc comment.
 */
function useHasEditPermission(projectId: string): boolean {
  const query = usePermissionList(projectId, { query: { enabled: !!projectId } });
  return useMemo(() => {
    const list = query.data?.data as Permission[] | undefined;
    if (!list) return false;
    return list.some((entry) => entry.enabled && entry.name === PERMISSIONS.projectContext.edit);
  }, [query.data]);
}

export function GenerateProjectContextButton({
  projectId,
  existingContent,
  onApply,
  openAiOnMount = false,
}: GenerateProjectContextButtonProps): ReactNode {
  const hasEditPermission = useHasEditPermission(projectId);
  const [isOpen, setIsOpen] = useState(false);

  const handleOpen = useCallback(() => setIsOpen(true), []);
  const handleClose = useCallback(() => setIsOpen(false), []);

  /*
   * Opens ONCE per rising edge, and only while the caller may edit — the two
   * early returns below unmount this component entirely for a viewer without
   * the permission or a deployment without the draft endpoint, so a request
   * to auto-open simply never arrives there. Closing the dialog must not
   * re-open it, which is why this depends on the request flag and not on
   * `isOpen`.
   */
  useEffect(() => {
    if (openAiOnMount) setIsOpen(true);
  }, [openAiOnMount]);

  // The draft endpoint is not mounted. See `shared/config/backendCapabilities`.
  if (!hasBackendCapability('aiGeneration')) return null;
  if (!hasEditPermission) return null;

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
        {t('entities.projectContext.generateButton.label', 'Generate with AI')}
      </BaseBtn>
      <GenerateProjectContextModal
        open={isOpen}
        onClose={handleClose}
        projectId={projectId}
        existingContent={existingContent}
        onApply={onApply}
      />
    </>
  );
}

const buttonSx: SxProps<Theme> = (theme: Theme) => ({
  borderRadius: theme.vars.shape.radiusPill,
  color: theme.vars.palette.primary.main,
});
