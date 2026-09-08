/**
 * AgentPipelineBuilder — the "Agent & Pipeline Builder" switch inside
 * Settings › General.
 *
 * Ported from `EliteaUI/src/[fsd]/features/settings/ui/project-general/
 * AgentPipelineBuilder.jsx`. The reference hosts it inside
 * `SettingsFormProvider`'s Formik form and saves on blur; this app has no
 * such host on this page, and a single switch does not need one — the write
 * fires on change, which is what "save on blur" amounts to for a control
 * with no intermediate state.
 *
 * The transport is deliberately the same one `ui/ai-personality/
 * SettingsFormProvider.tsx` uses for the same endpoint: the plain
 * `updateCurrentAuthor` call plus an explicit invalidation of
 * `getGetCurrentAuthorQueryKey()`. Two writers of one blob that disagree
 * about how to write it is how a carry-forward gets missed.
 *
 * `lib/project-general/agentPipelineBuilder.ts` explains where the flag is
 * stored and why the whole `personalization` blob is rewritten every time.
 */
import { memo, useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useQueryClient } from '@tanstack/react-query';

import { t } from '@/shared/i18n';
import {
  getGetCurrentAuthorQueryKey,
  updateCurrentAuthor,
  useGetCurrentAuthor,
} from '@/shared/api/generated/social/social';

import { EnableToggleCard } from '../project-context/EnableToggleCard';
import {
  type AuthorWithPersonalization,
  buildInternalMcpUpdate,
  selectInternalMcpEnabled,
} from '../../lib/project-general/agentPipelineBuilder';

export interface AgentPipelineBuilderProps {
  /** `PERMISSIONS.projectContext.edit` — the reference gates this control on it. */
  readonly canEdit: boolean;
}

export const AgentPipelineBuilder = memo(function AgentPipelineBuilder({
  canEdit,
}: AgentPipelineBuilderProps) {
  const queryClient = useQueryClient();
  const { data } = useGetCurrentAuthor();
  const author = data?.data as AuthorWithPersonalization | undefined;
  const [isSaving, setIsSaving] = useState(false);

  const enabled = selectInternalMcpEnabled(author);

  const handleToggle = useCallback(
    (checked: boolean) => {
      if (!canEdit || isSaving) return;
      const write = async () => {
        setIsSaving(true);
        try {
          await updateCurrentAuthor(buildInternalMcpUpdate(author, checked));
          // The switch reads its position straight off the profile query, so
          // that query is what has to be refreshed. Without this the control
          // shows the pre-save value until something else happens to refetch.
          await queryClient.invalidateQueries({ queryKey: getGetCurrentAuthorQueryKey() });
        } finally {
          setIsSaving(false);
        }
      };
      void write();
    },
    [author, canEdit, isSaving, queryClient],
  );

  return (
    <Box sx={bodySx}>
      <EnableToggleCard
        enabled={enabled}
        onToggle={handleToggle}
        disabled={!canEdit || isSaving}
        title={t('settings.projectGeneral.agentPipelineBuilder.title', 'Agent & Pipeline Builder')}
        description={t(
          'settings.projectGeneral.agentPipelineBuilder.description',
          'Create and update agents and pipelines directly from chat.',
        )}
      />
    </Box>
  );
});

const bodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
  width: '100%',
};
