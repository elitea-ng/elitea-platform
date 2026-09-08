import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useFormContext } from 'react-hook-form';

import type { ApplicationCreationInput } from '@/entities/application-form';
import { AgentVersionControls } from '@/features/agents';
import { useLivePipelineGraphAdmission, usePipelineGraphDraft } from '@/features/pipelines';
import type { ApplicationVersionDetail, ApplicationVersionSummary } from '@/shared/api/generated/model';

import type { EditPipelineVersionFields } from '../lib/useEditPipelineVersionFields';
import { usePipelineVersionControls } from '../lib/usePipelineVersionControls';

const wrapperSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.75rem' };

export interface EditPipelineVersionBarProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly tab: string | undefined;
  readonly versions: readonly ApplicationVersionSummary[];
  readonly activeVersion: ApplicationVersionDetail | undefined;
  /** Public-project viewer — the selector stays, the write affordances go. */
  readonly isReadOnly: boolean;
  readonly isFetching: boolean;
  /** The live version-level edits, which a cloned version inherits over the stored blob. */
  readonly versionFields: EditPipelineVersionFields;
}

/**
 * The pipeline editor's version bar — the pipelines-side mount point for
 * `features/agents`' `AgentVersionControls` (version dropdown + Set-as-default
 * + Delete version + Save As Version).
 *
 * **Why a component rather than inline JSX in `EditPipeline.tsx`.** Two
 * reasons, both structural. `useFormContext` and `usePipelineGraphDraft` are
 * read here so the page does not have to thread a `control` and a graph
 * reader through props: `EditPipeline` both CREATES the RHF instance and
 * renders `<FormProvider>`, so it sits above its own provider and cannot call
 * a context hook itself — the same constraint `EditPipelineSaveBar.tsx`
 * already documents for `useDiscardPipelineChanges`. And the page is at the
 * §3.5 400-line ceiling; every decision this file makes would otherwise have
 * to be made there.
 *
 * The error line covers BOTH version-write failures — the create POST's (via
 * `onNewVersionError`) and the follow-up graph PUT's — because this app has
 * no toast infrastructure and the page's own `role="alert"` banner sits in
 * the content area, below the fold of the bar the user just clicked in.
 */
export function EditPipelineVersionBar({
  projectId,
  applicationId,
  tab,
  versions,
  activeVersion,
  isReadOnly,
  isFetching,
  versionFields,
}: EditPipelineVersionBarProps): ReactNode {
  const { control } = useFormContext<ApplicationCreationInput>();
  const readGraphDraft = usePipelineGraphDraft();
  /*
   * Read HERE rather than on the page: this component is small and re-renders
   * cheaply, whereas subscribing `EditPipeline` to `yamlCode` would re-render
   * the whole editor tree on every keystroke — the same reason
   * `usePipelineGraphDraft` hands back a reader instead of a value.
   *
   * `formState.isValid` is deliberately NOT the signal used here. The save
   * gate publishes its veto as an RHF `root.*` error, and that error can be
   * absent while the graph is still inadmissible: `ConfigurationTab` unmounts
   * the gate whenever the detail refetch errors, and a resolver pass drops
   * `root.*` errors wholesale. The live document is the durable answer.
   */
  const { isAdmissible } = useLivePipelineGraphAdmission();

  const controls = usePipelineVersionControls({
    projectId,
    applicationId,
    tab,
    versions,
    activeVersion,
    control,
    versionFields,
    readGraphDraft,
    isReadOnly,
    isFetching,
    isGraphAdmissible: isAdmissible,
  });

  if (!controls.showVersionControls) return null;

  return (
    <Box sx={wrapperSx}>
      {controls.versionError !== undefined && (
        <Typography
          role="alert"
          variant="bodySmall"
        >
          {controls.versionError}
        </Typography>
      )}
      <AgentVersionControls
        applicationId={controls.applicationIdText}
        projectId={projectId}
        versions={controls.versionOptions}
        activeVersionId={controls.activeVersionId}
        onSelectVersion={controls.handleSelectVersion}
        versionBody={controls.versionBody}
        canSaveNewVersion={controls.canSaveNewVersion}
        saveNewVersionDisabled={controls.isSaveNewVersionBlocked}
        versionDelete={controls.versionDelete}
        /* The delete refusal's own channel — see `usePipelineVersionControls`. */
        onNewVersionSaved={controls.handleNewVersionSaved}
        onNewVersionError={controls.reportVersionError}
      />
    </Box>
  );
}
