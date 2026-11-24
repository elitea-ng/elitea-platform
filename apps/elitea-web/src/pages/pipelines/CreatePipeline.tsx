import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { zodResolver } from '@hookform/resolvers/zod';
import { useNavigate, useParams } from '@tanstack/react-router';
import { FormProvider, useForm } from 'react-hook-form';

import {
  applicationCreationSchema,
  CreateApplicationTabBar,
  useCreateApplicationDraft,
  useCreateApplicationInitialValues,
  type ApplicationCreationInput,
} from '@/entities/application-form';
import { applicationServerErrorMessage, CreateAgentForm } from '@/features/agents';
import {
  ConfigurationTab,
  resetPipelineDraft,
  useLivePipelineGraphAdmission,
  usePipelineGraphDraft,
  usePipelineVersionSync,
  type ConfigurationTabProps,
} from '@/features/pipelines';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { t } from '@/shared/i18n';
import { usePageCrumbs } from '@/shared/ui/page-chrome';
import { AgentModelSettings } from '@/widgets/agent-model-settings';
import { disarmUnsavedChangesNavBlocker, useUnsavedChangesNavBlocker } from '@/widgets/app-shell';
import { PageToolbar, pageSx } from '@/widgets/page-header';

import {
  areExtraFieldsEqual,
  CREATE_DRAFT_VERSION_ID,
  NOOP_CREATE_CHAT_ADAPTER,
  pipelineInstructions,
  type CreatePipelineFormExtraFields,
} from './lib/createPipeline.helpers';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { CreatePipelineAdmissionAlert } from './ui/CreatePipelineAdmissionAlert';

const workspaceSx: SxProps<Theme> = {
  flex: 1,
  minHeight: 0,
  display: 'flex',
  flexDirection: 'column',
  overflow: 'hidden',
};

const chatLockedSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  height: '100%',
  padding: theme.spacing(3),
  textAlign: 'center',
});

/**
 * Canvas-first Create Pipeline page (`/pipelines/create`, Gap #12).
 *
 * Mounts the live `ConfigurationTab` workspace (left configuration form + center
 * interactive `FlowEditor` canvas with `FlowEmptyState` when cleared + locked
 * test pane) while preserving `CreatePipelineAdmissionAlert`, live/save-time
 * admission checks, starter-template seeding, and the `#133` unsaved-changes guard.
 */
export function CreatePipeline(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as { tab?: string };
  const projectId = useSelectedProjectId();
  const draftDefaults = useCreateApplicationInitialValues(true);
  const { create, isCreating, error: createRequestError } = useCreateApplicationDraft(projectId);
  const [createError, setCreateError] = useState<unknown>(undefined);
  const [isCanvasYamlDirty, setIsCanvasYamlDirty] = useState(false);
  const isCanvasYamlDirtyRef = useRef(false);
  isCanvasYamlDirtyRef.current = isCanvasYamlDirty;
  const readGraphDraft = usePipelineGraphDraft();

  const form = useForm<ApplicationCreationInput>({
    resolver: zodResolver(applicationCreationSchema),
    mode: 'onChange',
    defaultValues: {
      name: draftDefaults.name,
      description: draftDefaults.description,
      version_details: { conversation_starters: [...draftDefaults.versionDetails.conversationStarters] },
    },
  });

  const [extraFields, setExtraFields] = useState<CreatePipelineFormExtraFields>({
    instructions: pipelineInstructions(draftDefaults.versionDetails.instructions),
    welcomeMessage: '',
    variables: draftDefaults.versionDetails.variables.map((variable) => ({ ...variable })),
    stepLimit: draftDefaults.versionDetails.meta.step_limit,
    llmSettings: draftDefaults.versionDetails.llmSettings,
  });

  const initialExtraFields = useRef(extraFields);
  const isDraftDirty =
    form.formState.isDirty ||
    isCanvasYamlDirty ||
    !areExtraFieldsEqual(extraFields, initialExtraFields.current);
  useUnsavedChangesNavBlocker(isDraftDirty);

  useEffect(() => () => {
    resetPipelineDraft();
  }, []);

  const syncVersionDetails = useMemo(
    () =>
      ({
        id: CREATE_DRAFT_VERSION_ID,
        instructions: extraFields.instructions,
      }) as unknown as ApplicationVersionDetail,
    [extraFields.instructions],
  );

  usePipelineVersionSync({
    isCreateMode: false,
    versionDetails: syncVersionDetails,
    versionId: CREATE_DRAFT_VERSION_ID,
  });

  const admission = useLivePipelineGraphAdmission(
    isCanvasYamlDirty ? undefined : pipelineInstructions(extraFields.instructions),
  );

  const name = form.watch('name') ?? '';
  const description = form.watch('description') ?? '';
  const rawConversationStarters = form.watch('version_details.conversation_starters');

  const pipelineDraftValues = {
    name,
    description,
    version_details: {
      instructions: extraFields.instructions,
      welcome_message: extraFields.welcomeMessage,
      variables: extraFields.variables,
      meta: { step_limit: extraFields.stepLimit },
      llm_settings: extraFields.llmSettings,
    },
  };

  const tabVersionDetails = useMemo<ConfigurationTabProps['versionDetails']>(
    () =>
      ({
        id: CREATE_DRAFT_VERSION_ID,
        type: 'chat',
        tools: [],
        conversation_starters: (rawConversationStarters ?? []).filter((entry): entry is string => typeof entry === 'string'),
        ...(extraFields.llmSettings ? { llm_settings: { ...extraFields.llmSettings } } : {}),
      }) as unknown as ConfigurationTabProps['versionDetails'],
    [rawConversationStarters, extraFields.llmSettings],
  );

  const handlePipelineFieldChange = useCallback(
    (path: string, value: unknown) => {
      switch (path) {
        case 'name':
          form.setValue('name', typeof value === 'string' ? value : '', { shouldValidate: true, shouldDirty: true });
          return;
        case 'description':
          form.setValue('description', typeof value === 'string' ? value : '', {
            shouldValidate: true,
            shouldDirty: true,
          });
          return;
        case 'version_details.instructions':
          setIsCanvasYamlDirty(false);
          setExtraFields((previous) => ({ ...previous, instructions: typeof value === 'string' ? value : '' }));
          return;
        case 'version_details.welcome_message':
          setExtraFields((previous) => ({ ...previous, welcomeMessage: typeof value === 'string' ? value : '' }));
          return;
        case 'version_details.variables':
          setExtraFields((previous) => ({
            ...previous,
            variables: Array.isArray(value) ? (value as { name: string; value: string }[]) : previous.variables,
          }));
          return;
        case 'version_details.meta.step_limit':
          setExtraFields((previous) => ({
            ...previous,
            stepLimit: typeof value === 'number' ? value : undefined,
          }));
          return;
        default:
          return;
      }
    },
    [form],
  );

  const handleSave = useCallback(() => {
    const graphDraft = isCanvasYamlDirtyRef.current ? readGraphDraft() : undefined;
    const effectiveAdmission = graphDraft ? graphDraft.admission : admission;
    if (!effectiveAdmission.isAdmissible) return;
    const effectiveInstructions = pipelineInstructions(graphDraft?.instructions ?? extraFields.instructions);

    void form.handleSubmit(async (values) => {
      setCreateError(undefined);
      const created = await create({
        name: values.name,
        description: values.description,
        type: draftDefaults.type,
        version: {
          ...draftDefaults.versionDetails,
          instructions: effectiveInstructions,
          welcomeMessage: extraFields.welcomeMessage,
          conversationStarters: (values.version_details?.conversation_starters ?? []).filter(
            (entry): entry is string => typeof entry === 'string',
          ),
          variables: extraFields.variables,
          meta: {
            ...draftDefaults.versionDetails.meta,
            step_limit: extraFields.stepLimit ?? draftDefaults.versionDetails.meta.step_limit,
          },
          llmSettings: extraFields.llmSettings,
          ...(graphDraft?.pipelineSettings ? { pipelineSettings: graphDraft.pipelineSettings } : {}),
        },
      });
      if (created === undefined) {
        setCreateError(new Error('createFailed'));
        return;
      }
      resetPipelineDraft();
      disarmUnsavedChangesNavBlocker();
      void navigate({
        to: '/pipelines/$tab/$agentId',
        params: { tab: params.tab ?? 'latest', agentId: created.id },
        search: { isFromCreation: 'true' },
      });
    })();
  }, [admission, create, draftDefaults, extraFields, form, navigate, params.tab, readGraphDraft]);

  const handleModelSettingsChange = useCallback(
    (next: AgentLlmSettings) => setExtraFields((previous) => ({ ...previous, llmSettings: next })),
    [],
  );

  const handleCancel = useCallback(() => {
    resetPipelineDraft();
    disarmUnsavedChangesNavBlocker();
    void navigate({ to: '/pipelines/$tab', params: { tab: params.tab ?? 'latest' } });
  }, [navigate, params.tab]);

  usePageCrumbs([{ label: t('pages.pipelines.createPipeline.title', 'New Pipeline') }]);

  return (
    <FormProvider {...form}>
      <Box sx={pageSx}>
        <PageToolbar
          secondaryActions={
            <CreateApplicationTabBar
              onSave={handleSave}
              onCancel={handleCancel}
              canSave={form.formState.isValid && !isCreating && admission.isAdmissible}
              isSaving={isCreating}
              saveTestId="pipeline-save-button"
            />
          }
        />
        <Box sx={workspaceSx} data-testid="create-pipeline-workspace">
          <ConfigurationTab
            isFetching={false}
            isError={false}
            applicationId={undefined}
            pipelineName={name}
            versionDetails={tabVersionDetails}
            setFieldValue={handlePipelineFieldChange}
            setYamlDirty={setIsCanvasYamlDirty}
            adapter={NOOP_CREATE_CHAT_ADAPTER}
            slots={{
              renderConfigurationForm: () => (
                <Box>
                  {createError !== undefined && (
                    <Typography role="alert" variant="bodyMedium">
                      {applicationServerErrorMessage(
                        createRequestError,
                        t('pages.pipelines.createPipeline.error', 'Failed to create the pipeline.'),
                      )}
                    </Typography>
                  )}
                  <CreatePipelineAdmissionAlert admission={admission} />
                  <Box data-testid="create-pipeline-form-panel">
                    <CreateAgentForm
                      entityType="pipeline"
                      values={pipelineDraftValues}
                      onFieldChange={handlePipelineFieldChange}
                      disabled={isCreating}
                      modelSettingsSlot={
                        <AgentModelSettings
                          projectId={projectId}
                          value={extraFields.llmSettings}
                          onChange={handleModelSettingsChange}
                          disabled={isCreating}
                        />
                      }
                    />
                  </Box>
                </Box>
              ),
              renderChat: () => (
                <Box sx={chatLockedSx} data-testid="create-pipeline-chat-locked">
                  <Typography variant="bodySmall" color="text.secondary">
                    {t('pages.pipelines.createPipeline.saveToTest', 'Save the pipeline to test it.')}
                  </Typography>
                </Box>
              ),
            }}
          />
        </Box>
      </Box>
    </FormProvider>
  );
}
