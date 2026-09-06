import { useCallback, useRef, useState, type ReactNode } from 'react';

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
import { CreateAgentForm } from '@/features/agents';
import { PIPELINE_STARTER_TEMPLATE } from '@/shared/lib/pipelineStarterTemplate';
import { areAgentLlmSettingsEqual, type AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { t } from '@/shared/i18n';
import { AgentModelSettings } from '@/widgets/agent-model-settings';
import { disarmUnsavedChangesNavBlocker, useUnsavedChangesNavBlocker } from '@/widgets/app-shell';

import { useSelectedProjectId } from './lib/useSelectedProjectId';

/**
 * The `version_details` subset `CreateAgentForm` reads/writes that
 * `applicationCreationSchema` (name/description/conversation_starters only)
 * does not validate — held as local state rather than widening the RHF form's
 * generic. Same adapter, same reasoning as
 * `pages/agents/CreateApplication.tsx`'s `CreateAgentFormExtraFields`.
 */
interface CreatePipelineFormExtraFields {
  readonly instructions: string;
  readonly welcomeMessage: string;
  readonly variables: { readonly name: string; readonly value: string }[];
  readonly stepLimit: number | undefined;
  /** The model this pipeline's first version will run on; `undefined` leaves it on the project catalogue default. */
  readonly llmSettings: AgentLlmSettings | undefined;
}

/**
 * The `extraFields` half of "is this draft dirty?" (#133) — RHF's
 * `formState.isDirty` only sees the three schema-validated fields, so a user
 * who typed only instructions would otherwise lose that work unprompted.
 *
 * Duplicated from `pages/agents/CreateApplication.tsx`'s
 * `areExtraFieldsEqual` rather than shared: `entities/application-form`'s
 * barrel — the only slice both pages could legally reach for it — is at its
 * §3.5 export budget (exactly 20, its own header says so), and a
 * `pages/pipelines` -> `pages/agents` import is a sideways page import. The
 * whole surrounding adapter is already deliberately duplicated between these
 * two files for the same reason (see this file's own header).
 */
function areExtraFieldsEqual(a: CreatePipelineFormExtraFields, b: CreatePipelineFormExtraFields): boolean {
  if (a.instructions !== b.instructions) return false;
  if (a.welcomeMessage !== b.welcomeMessage) return false;
  if (a.stepLimit !== b.stepLimit) return false;
  // Key by key, never by identity: the picker hands back a fresh object each
  // time, so an identity check would report "dirty" from the first render.
  if (!areAgentLlmSettingsEqual(a.llmSettings, b.llmSettings)) return false;
  if (a.variables.length !== b.variables.length) return false;
  return a.variables.every((variable, index) => {
    const other = b.variables[index];
    return other !== undefined && variable.name === other.name && variable.value === other.value;
  });
}

/**
 * The YAML graph a new pipeline is STORED with.
 *
 * `instructions` IS the pipeline's graph: `usePipelineVersionSync` parses the
 * saved string and seeds both the YAML pane and the canvas from it, and
 * `pipeline_settings` only supplies node geometry on top. An empty string
 * therefore stored a pipeline with no graph, and Create pipeline -> Save
 * opened the editor on a blank document with `End` alone on the canvas. It was
 * also unrunnable: the compiler refuses an empty document, so the first chat
 * turn failed in another process with no signal on this screen.
 *
 * The fallback fires only on a document the user did not author. Anything
 * typed on this page wins, so this can never overwrite real work — and the
 * seed stays out of the create FORM's own state, which is why the Instructions
 * control still opens empty and the unsaved-changes guard (#133) still reads
 * a fresh page as clean.
 *
 * `usePipelineEditorCreate` applies the same template to the chat surface's
 * create path. This page is the other one, and it is the one the UI actually
 * takes.
 */
function pipelineInstructions(authored: string): string {
  return authored.trim() === '' ? PIPELINE_STARTER_TEMPLATE : authored;
}

const pageSx: SxProps<Theme> = {
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
};

const tabBarSx: SxProps<Theme> = {
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  borderBottom: 1,
  borderColor: 'divider',
  padding: '0 1.5rem',
  minHeight: '3rem',
};

const contentSx: SxProps<Theme> = {
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
  padding: '1.5rem',
};

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/CreatePipeline.jsx` —
 * ROUTE-018 `/pipelines/create` (spec §8.1). Structurally the
 * pipelines-domain mirror of `pages/agents/CreateApplication.tsx` (Wave-2
 * unit A1g) — a Pipeline literally IS an Application row
 * (`agent_type: 'pipeline'`) — differing only in `useCreateApplicationInitialValues(true)`
 * (forPipeline) and the `/pipelines/*` route targets.
 *
 * **Disclosed Formik->RHF redesign**, same reasoning `pages/agents/
 * CreateApplication.tsx` and the split `entities/application-form`
 * components already establish: the baseline wraps everything in a `Formik`
 * whose `onSubmit` is a no-op — the real "create" call lives inside
 * `CreateAgentForm`'s own `useFormikContext()`/`useCreateApplication(formik)`
 * reads (`entityType="pipeline"`, `CreatePipeline.jsx:78-83`). This page owns
 * a `useForm` + `FormProvider` instance instead.
 *
 * **Dropped, disclosed:** the baseline's `useEffect` dispatching
 * `actions.initThePipeline({nodes: [], edges: [], yamlJsonObject: {state:
 * FlowEditorConstants.DefaultState}, yamlCode: '', layout_version:
 * FlowEditorConstants.LAYOUT_VERSION})` and `editorActions.
 * resetPipelineEditor()` (`CreatePipeline.jsx:20-33`) resets the Redux
 * pipeline-editor slices before the create form mounts. Its zustand
 * equivalent, `features/pipelines/model/pipelineEditorStore.ts`'s
 * `resetPipelineEditor()`, is not exported from `features/pipelines/
 * index.ts` as of this unit's landing (verified — same gap `useSavePipeline.ts`,
 * this unit, cites in full), so there is no R-L3-legal way to call it from
 * `pages/pipelines`. `useCreateApplicationInitialValues(true)`
 * (`entities/application-form`) already seeds an empty
 * `pipelineSettings: {nodes: [], edges: []}` draft independent of that
 * store, so the create FORM's own initial state is not affected by this
 * gap — only a currently-mounted flow-editor widget's own state (owned by a
 * sibling A2 sub-unit) would be.
 *
 * **Composition gap, CLOSED.** This page previously rendered
 * `<Box data-testid="create-pipeline-form-panel" />` — a self-closing, empty
 * element — on the stated grounds that (a) `features/agents` had no public
 * `CreateAgentForm` export, and (b) `no-sideways-features` would forbid the
 * import anyway. Both were stale by the time the E2E suite first ran and
 * failed here (J16: the testid resolved 24× to an empty div, never visible):
 *
 *  - `CreateAgentForm` IS a public export of `features/agents`
 *    (`features/agents/index.ts:32`), and `pages/agents/CreateApplication.tsx`
 *    already imports it.
 *  - `no-sideways-features` is scoped `from: ^src/features/([^/]+)/`
 *    (`.dependency-cruiser.cjs:54`). This file is a PAGE, not a feature, and
 *    `page -> feature` is the ordinary layer direction (§3.2).
 *
 * The adapter below mirrors `pages/agents/CreateApplication.tsx`'s: the RHF
 * form owns name/description (the only fields `applicationCreationSchema`
 * validates) and local state owns the version_details fields the schema does
 * not cover, exactly as the agents page does and for the same reason.
 */
export function CreatePipeline(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as { tab?: string };
  const projectId = useSelectedProjectId();
  const draftDefaults = useCreateApplicationInitialValues(true);
  const { create, isCreating } = useCreateApplicationDraft(projectId);
  const [createError, setCreateError] = useState<unknown>(undefined);

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
    instructions: draftDefaults.versionDetails.instructions,
    welcomeMessage: '',
    variables: draftDefaults.versionDetails.variables.map((variable) => ({ ...variable })),
    stepLimit: draftDefaults.versionDetails.meta.step_limit,
    llmSettings: draftDefaults.versionDetails.llmSettings,
  });

  /*
   * #133 — arm the app-wide unsaved-changes guard, exactly as
   * `pages/agents/CreateApplication.tsx` now does. `widgets/app-shell`'s
   * `NavBlockerDialog` was mounted under this page all along but nothing on
   * the standalone `/pipelines` editors ever raised the flag, so a typed
   * draft was thrown away on any nav-link click. `useRef`'s initialiser is
   * kept only from the first render, so this holds the opening values.
   */
  const initialExtraFields = useRef(extraFields);
  const isDraftDirty = form.formState.isDirty || !areExtraFieldsEqual(extraFields, initialExtraFields.current);
  useUnsavedChangesNavBlocker(isDraftDirty);

  const name = form.watch('name') ?? '';
  const description = form.watch('description') ?? '';

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
        // No `version_details.llm_settings` case, deliberately: model settings
        // reach `extraFields.llmSettings` through `handleModelSettingsChange`
        // below, straight off the `AgentModelSettings` slot. The create form's
        // dispatcher (`features/agents/model/useCreateAgentFormState.ts`)
        // emits only the paths above, so a branch here would be dead — the
        // EDIT page's identical-looking branch IS live, which is what made
        // this one read as reachable. Do not re-add it.
        default:
          return;
      }
    },
    [form],
  );

  const handleSave = useCallback(() => {
    void form.handleSubmit(async (values) => {
      setCreateError(undefined);
      const created = await create({
        name: values.name,
        description: values.description,
        type: draftDefaults.type,
        /*
         * Every field the page HOLDS, not just the schema-validated ones.
         * This used to spread `draftDefaults.versionDetails` and override
         * `conversationStarters` alone, with `extraFields` absent from both
         * the body and this callback's dependency list — so the instructions,
         * variables and step limit a user typed on this page were replaced by
         * the empty draft defaults on Save, silently, with a 201 back. The
         * agents twin (`pages/agents/CreateApplication.tsx`) always did it
         * this way; only the pipelines copy drifted.
         */
        version: {
          ...draftDefaults.versionDetails,
          // Never the raw field: a blank one stores a pipeline no runtime can
          // run and no editor can show. See `pipelineInstructions`.
          instructions: pipelineInstructions(extraFields.instructions),
          // The same key the agents twin was missing: held in `extraFields`,
          // echoed back into the form, and absent from this body — so a
          // pipeline's welcome message was dropped on create with a 201 back.
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
        },
      });
      if (created === undefined) {
        setCreateError(new Error('createFailed'));
        return;
      }
      // #133: the draft is persisted — this is the SAVE's own navigation,
      // not a nav-away from unsaved work.
      disarmUnsavedChangesNavBlocker();
      void navigate({
        to: '/pipelines/$tab/$agentId',
        params: { tab: params.tab ?? 'latest', agentId: created.id },
        search: { isFromCreation: 'true' },
      });
    })();
  }, [form, create, draftDefaults, extraFields, navigate, params.tab]);

  const handleModelSettingsChange = useCallback(
    (next: AgentLlmSettings) => setExtraFields((previous) => ({ ...previous, llmSettings: next })),
    [],
  );

  const handleCancel = useCallback(() => {
    // #133: Cancel IS the explicit discard, so it is not also prompted.
    disarmUnsavedChangesNavBlocker();
    void navigate({ to: '/pipelines/$tab', params: { tab: params.tab ?? 'latest' } });
  }, [navigate, params.tab]);

  return (
    <FormProvider {...form}>
      <Box sx={pageSx}>
        <Box sx={tabBarSx}>
          <Typography variant="headingSmall">{t('pages.pipelines.createPipeline.title', 'New Pipeline')}</Typography>
          <CreateApplicationTabBar
            onSave={handleSave}
            onCancel={handleCancel}
            canSave={form.formState.isValid && !isCreating}
            isSaving={isCreating}
            saveTestId="pipeline-save-button"
          />
        </Box>
        <Box sx={contentSx}>
          {createError !== undefined && (
            <Typography
              role="alert"
              variant="bodyMedium"
            >
              {t('pages.pipelines.createPipeline.error', 'Failed to create the pipeline.')}
            </Typography>
          )}
          <Box data-testid="create-pipeline-form-panel">
            <CreateAgentForm
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
      </Box>
    </FormProvider>
  );
}
