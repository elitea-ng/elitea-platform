/**
 * The publish surface as ONE component: the two toolbar controls, the dialog
 * behind them, the unpublish confirmation, and the error seam for the actions
 * that happen with no dialog open.
 *
 * The editor page used to hold all three pieces — the hook, the buttons' props
 * and the modal — and thread them together itself. That put a three-part
 * protocol in a page whose subject is editing a skill, and it put four more
 * symbols across the slice boundary for no gain: a caller has no decision to
 * make between "run the hook" and "render the dialog", so there was nothing for
 * it to compose.
 *
 * `permissions` is passed IN rather than read here. The permission set comes
 * from `widgets/sidebar`, and a feature reaching into a widget is the wrong
 * direction — the same layering rule `pages/agents/lib/isPublicAgentsProject.ts`
 * documents for `routes/`.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Snackbar from '@mui/material/Snackbar';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { publishTargetOf } from '../lib/publishTarget';
import { useSkillPublishing } from '../model/useSkillPublishing';
import { PublishSkillModal } from './PublishSkillModal';
import { SkillPublishButtons } from './SkillPublishButtons';
import type { SkillRecord } from '../model/types';

export interface SkillPublishControlsProps {
  readonly projectId: string | undefined;
  readonly skill: SkillRecord | undefined;
  /** The skill's route id, which is not always the record's own. */
  readonly skillId: string | undefined;
  /** The version the URL selects, if any. */
  readonly versionId: string | undefined;
  readonly permissions: ReadonlySet<string>;
  /** Called after a publish or unpublish lands, to refetch what changed. */
  readonly onPublished: () => void;
}

export function SkillPublishControls({
  projectId,
  skill,
  skillId,
  versionId,
  permissions,
  onPublished,
}: SkillPublishControlsProps): ReactNode {
  const state = useSkillPublishing(
    projectId,
    publishTargetOf(skill, skillId, versionId),
    permissions,
    onPublished,
  );
  const [confirmingUnpublish, setConfirmingUnpublish] = useState(false);

  return (
    <>
      <SkillPublishButtons
        canShowPublish={state.canShowPublish}
        canPublish={state.canPublish}
        canUnpublish={state.canUnpublish}
        isUnpublishing={state.isUnpublishing}
        onPublish={state.open}
        onUnpublish={() => setConfirmingUnpublish(true)}
      />
      <PublishSkillModal state={state} />
      {/*
       * Unpublish confirms first, like every other destructive control in this
       * app. It is not a soft delete: the catalog copy is DELETED, and the twin
       * skill goes with it when that was its last published version, so a
       * mis-click on a button sitting next to Export and Delete costs the
       * catalog entry and its identity. Republishing makes a new one.
       */}
      <DeleteEntityModal
        open={confirmingUnpublish}
        {...(skill ? { name: skill.name } : {})}
        confirming={state.isUnpublishing}
        onClose={() => setConfirmingUnpublish(false)}
        onConfirm={() => {
          setConfirmingUnpublish(false);
          void state.unpublish();
        }}
        copy={{
          title: t('skills.unpublish.title', 'Unpublish skill'),
          textContent: t('skills.unpublish.text', 'Remove the public catalog copy of '),
          confirmText: t('skills.unpublish.confirm', 'Unpublish'),
        }}
        content={{
          extra: (
            <Alert
              severity="warning"
              sx={{ mt: 2 }}
            >
              {t(
                'skills.unpublish.warning',
                'The published copy is deleted, not hidden. Publishing again creates a new catalog entry.',
              )}
            </Alert>
          ),
        }}
      />
      {/*
       * The error seam for unpublish.
       *
       * `state.error` is rendered inside the publish dialog, and unpublish runs
       * with that dialog CLOSED — so a refusal (403 from the permission gate,
       * 409 when the catalog copy is already gone) used to be written to state
       * nothing displayed: the button re-enabled, the version stayed published,
       * and the user saw exactly what a success looks like. This app has no
       * global toast host, so it uses a local Snackbar, as
       * `features/toolkits/ui/IndexesTab.tsx` does.
       */}
      <Snackbar
        open={state.error !== undefined && !state.isOpen}
        autoHideDuration={6000}
        onClose={state.dismissError}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      >
        <Alert
          severity="error"
          variant="filled"
          data-testid="skill-unpublish-error"
          onClose={state.dismissError}
        >
          {state.error}
        </Alert>
      </Snackbar>
    </>
  );
}
