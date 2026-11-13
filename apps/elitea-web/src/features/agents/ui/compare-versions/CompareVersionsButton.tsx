import type { ReactNode } from 'react';

import DifferenceOutlinedIcon from '@mui/icons-material/DifferenceOutlined';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

import type { AgentPipelineVersionOption } from '../../lib/types';
import { useApplicationsStore } from '../../model/applicationsStore';
import { CompareVersionsModal } from './CompareVersionsModal';

/**
 * The compare-versions trigger, mounted on the agent editor's version bar
 * (`../AgentVersionControls.tsx`) — where the baseline offers it, as an item
 * of the agent-actions dropdown (`ApplicationControls.jsx:169-179`).
 *
 * Gated on the same condition the baseline uses, `versions.length >= 2`:
 * there is nothing to compare a lone version against, and the picker would
 * open with an empty list. The baseline ALSO requires `applications.update`
 * — that gate belongs to its editable comparison panes, which this port does
 * not have (see `./CompareVersionsSteps.tsx`), so a read-only comparison is
 * offered to anyone who can already read the versions.
 *
 * #1997 — the open flag lives in `useApplicationsStore` so the editor's `⋮`
 * menu ("Compare versions", where the baseline and testers look for it) opens
 * this same modal; the icon stays as the one-click shortcut.
 */
export interface CompareVersionsButtonProps {
  readonly projectId: string | undefined;
  readonly applicationId: number;
  readonly versions: readonly AgentPipelineVersionOption[];
  readonly activeVersionId: number | undefined;
}

export function CompareVersionsButton(props: CompareVersionsButtonProps): ReactNode {
  const { projectId, applicationId, versions, activeVersionId } = props;
  const open = useApplicationsStore((state) => state.compareVersionsOpen);
  const setOpen = useApplicationsStore((state) => state.setCompareVersionsOpen);

  if (versions.length < 2 || activeVersionId === undefined) return null;

  return (
    <>
      <Tooltip title={t('features.agents.compareVersions.tooltip', 'Compare versions')}>
        <IconButton
          size="small"
          aria-label={t('features.agents.compareVersions.tooltip', 'Compare versions')}
          data-testid="compare-versions-button"
          onClick={() => setOpen(true)}
        >
          <DifferenceOutlinedIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      {open && (
        <CompareVersionsModal
          open={open}
          onClose={() => setOpen(false)}
          projectId={projectId}
          applicationId={applicationId}
          versions={versions}
          leftVersionId={activeVersionId}
        />
      )}
    </>
  );
}
