/**
 * The Indexes tab body — the mount point issue #149 is about.
 *
 * `features/toolkits/indexes/ui/IndexesContainer.tsx` is a fully ported,
 * fully tested composition root (list, selection, delete-confirm, the
 * "opened from a notification link but the index is gone" alert) that until
 * now had ZERO production importers: `pages/toolkits/EditToolkit.tsx`
 * rendered `{tab === 1 && <Box data-testid="edit-toolkit-indexes-tab-panel" />}`
 * — a real, clickable tab label in front of an empty Box.
 *
 * **Why this file, in `features/toolkits/ui/`, and not in `pages/`.**
 * `IndexesContainer` needs eleven injected dependencies. Eight are
 * INTRA-slice (`useToolkitChat`, `useGetSelectedToolSchema`,
 * `useGetCurrentToolkitSchemas`, `ToolFormContainer`, this slice's own
 * `useStopIndexingItemMutation`, `createToolkitConversation`,
 * `addToolkitConversationParticipant`, `buildToolkitMessagePayload`) and are
 * therefore reachable from here with plain relative imports (R-L3:
 * intra-slice imports are unrestricted) but NOT from `pages/`, which may
 * only enter a slice through its `index.ts` — and `features/toolkits`'
 * §3.5 public-API budget cannot afford eight more symbols. Three
 * (`LLMModelSelector` from `widgets/llm-model-selector`, `ChatMessageList`
 * from `features/chat-messages`, `ClearChatButton` from `widgets/chat`) are
 * OUTSIDE this slice and, for a `features/` file, permanently unreachable
 * (`no-upward-from-features` / `no-sideways-features`). So they stay
 * injected, exactly as `IndexChat.tsx`'s own DI treaty specifies, and
 * `pages/toolkits/EditToolkit.tsx` — which may legally import all three
 * barrels — supplies them.
 *
 * Net public-API cost: ONE symbol (`IndexesTab`), and nothing reaches
 * inside `indexes/**` from outside this file.
 *
 * CORRECTION — the DI seams were disclosed as unbuildable and no longer are.
 * `IndexChat.tsx` says `widgets/llm-model-selector` and `features/chat` do
 * not exist "anywhere in this app"; `IndexConfig.tsx` says "no
 * `features/toolkits/ui` directory exists yet"; `useToolkitChat.types.ts`
 * calls conversation-create / add-participant / list-models / indexing-stop
 * "no generated endpoint" gaps. Every one of those statements was true when
 * written and is false today — see `../api/toolkitChatSession.ts`'s own
 * CORRECTION block for the re-measured routes, and this file's imports for
 * the components. This is the "disclosed gap comments go stale silently"
 * failure mode; the stale sentences are left in place in their own files
 * (they are that unit's record) and superseded here.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * THE WORKER-CAPABILITY VERDICT (added 2026-09-07)
 * ─────────────────────────────────────────────────────────────────────────
 * `unavailableReason` is the served type schema's own
 * `metadata.unavailable_reason`, written by
 * `services/elitea-main/internal/api/v2/toolkits/type_catalogue.go:232-240`
 * whenever the worker cannot import the toolkit class at all. When it is set
 * this tab renders that sentence INSTEAD OF the indexes UI.
 *
 * Instead of, not beside: everything `IndexesContainer` offers ends in a run
 * on a worker that has already said it cannot run this type. A create form
 * whose `Index` button dispatches a job that dies in the worker, with the
 * answer sitting unread in the same schema the form was built from, is the
 * "absence reads as correctness" failure in its user-facing form. The list
 * is dropped with it: the read goes to the project's PgVector store, which
 * for an unrunnable type has never been written to, so it can only ever
 * confirm the emptiness the sentence already explains.
 *
 * NOT hidden, though — see `../lib/helpers/indexesTabVisibility.ts` for why
 * a vanishing tab explains less than a tab with a reason in it.
 *
 * STILL A REAL GAP, not papered over: `mcp_tokens` in the run payload — see
 * `../lib/helpers/toolkitMessagePayload.ts`'s own disclosure. It is
 * structurally unreachable from this slice and only affects `mcp`-type
 * toolkits, whose Indexes tab is not offered at all.
 */
import type { ComponentType, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Snackbar from '@mui/material/Snackbar';

import { t } from '@/shared/i18n';

import { addToolkitConversationParticipant, createToolkitConversation, useToolkitLlmModels } from '../api/toolkitChatSession';
import type { ToolkitLlmModel } from '../api/toolkitChatSession';
import { useStopIndexingItemMutation } from '../indexes/api/indexesApi';
import { useSelectedProjectId } from '../indexes/lib/hooks/useSelectedProjectId';
import type { IndexRow } from '../indexes/model/indexesStore';
import { IndexesContainer, type IndexesContainerProps } from '../indexes/ui/IndexesContainer';
import type { ChatMessageListProps, LLMModelSelectorProps } from '../indexes/ui/IndexDetails/IndexChat';
import type { UseToolkitChatParams, UseToolkitChatResult } from '../indexes/ui/IndexDetails/IndexDetails';
import type { EditToolDetail } from '../indexes/ui/IndexDetails/IndexActions';
import type { ToolFormFieldProps } from '../indexes/ui/IndexDetails/IndexConfig';
import { useGetCurrentToolkitSchemas } from '../lib/hooks/useGetCurrentToolkitSchemas.hooks';
import { buildToolkitMessagePayload } from '../lib/helpers/toolkitMessagePayload';
import { useToolkitChat } from '../lib/hooks/useToolkitChat.hooks';
import type { ToolkitChatIndexLike, ToolkitChatModel } from '../lib/hooks/useToolkitChat.types';
import { ToolFormContainer } from './form/ToolFormContainer';
import { useGetSelectedToolSchema } from './test-tools/useGetSelectedToolSchema';
import type { McpToolOption } from './test-tools/useGetSelectedToolSchema';

/**
 * Bridges `IndexConfig`'s `ToolFormFieldProps` (whole-object
 * `onChangeInputVariables(next)`) to `ToolFormContainer`'s single-field
 * `(fieldKey, value)` signature. That difference is not an accident — see
 * `ToolFormContainer.tsx`'s own DISCLOSED REDESIGN block: `shared/ui`'s
 * `Common*Field` family is single-field/controlled, so "the caller owns the
 * dict and does its own merge". This adapter IS that merge, and it is the
 * only place it needs to happen.
 */
function IndexToolFormField(props: ToolFormFieldProps): ReactNode {
  const { fieldKey, property, toolInputVariables, schema, onChangeInputVariables, changesDisabled } = props;

  const currentValues = useMemo(
    () => (typeof toolInputVariables === 'object' && toolInputVariables !== null ? (toolInputVariables as Readonly<Record<string, unknown>>) : {}),
    [toolInputVariables],
  );
  const handleChange = useCallback(
    (changedKey: string, value: unknown) => {
      onChangeInputVariables({ ...currentValues, [changedKey]: value });
    },
    [onChangeInputVariables, currentValues],
  );

  return (
    <ToolFormContainer
      fieldKey={fieldKey}
      property={property}
      toolInputVariables={currentValues}
      // `JsonSchemaLike.required` is `readonly string[] | undefined`;
      // `ToolFormContainerSchema.required` is optional, and
      // `exactOptionalPropertyTypes` forbids writing `undefined` into it.
      // Omitted rather than cast — `ToolFormContainer` reads `required` only
      // to decide whether a field is mandatory, and "no `required` array"
      // and "`required` is undefined" mean the same thing to it.
      schema={schema.required === undefined ? {} : { required: schema.required }}
      onChangeInputVariables={handleChange}
      {...(changesDisabled === undefined ? {} : { changesDisabled })}
    />
  );
}

/** `IndexRow.metadata` is an open `Record<string, unknown>`; `useToolkitChat` reads five named fields off it. Narrowed field-by-field rather than cast wholesale. */
function toChatIndex(index: IndexRow | null | undefined): ToolkitChatIndexLike | undefined {
  if (!index) return undefined;
  const meta = index.metadata;
  const str = (key: string): Record<string, string> => (typeof meta[key] === 'string' ? { [key]: meta[key] } : {});
  const config = meta['index_configuration'];
  return {
    id: index.id,
    metadata: {
      ...str('state'),
      ...str('conversation_id'),
      ...str('collection'),
      ...str('task_id'),
      ...(typeof config === 'object' && config !== null ? { index_configuration: config as Readonly<Record<string, unknown>> } : {}),
    },
  };
}

/** `ToolkitChatModel.project_id` is `string`; the models endpoint may answer a numeric one. Normalized once, here, so the same array feeds both `useToolkitChat` and `LLMModelSelector`. */
function toChatModel(model: ToolkitLlmModel): ToolkitChatModel & ToolkitLlmModel {
  return { ...model, project_id: String(model.project_id) };
}

export interface IndexesTabChatUI {
  /** `widgets/llm-model-selector` — see the module doc comment for why these three are injected and not imported. */
  readonly LLMModelSelector: ComponentType<LLMModelSelectorProps>;
  /** `features/chat-messages` */
  readonly ChatMessageList: ComponentType<ChatMessageListProps>;
  /** `widgets/chat` */
  readonly ClearChatButton: ComponentType<{ onClear: () => void }>;
}

export interface IndexesTabProps {
  readonly toolkitId: string;
  /** The toolkit's own `{type, settings}` — `IndexesContainer` forwards it to the run payload and the tool-schema lookup. */
  readonly values: Record<string, unknown>;
  /** The `IndexesToolsEnum` members present in the toolkit's `selected_tools` — gates the Run tab and the Remove-index action. */
  readonly selectedIndexTools: readonly string[];
  readonly chatUI: IndexesTabChatUI;
  /**
   * #308 — the index schedule modal's credential select.
   *
   * `IndexScheduleModal` declares this slot because `features/toolkits` may not
   * import `features/credentials` (`no-sideways-features`). It was threaded
   * down through `IndexesContainer`/`IndexDetails`/`IndexActions` and supplied
   * by no production caller, so the modal rendered no credential field and a
   * schedule that needs one could not be saved. The supplier is
   * `pages/toolkits/lib/credentialPickerSlots.tsx`.
   */
  readonly renderCredentialsSelect?: IndexesContainerProps['renderCredentialsSelect'];
  /**
   * The worker-capability verdict off the served type schema. Set means the
   * worker cannot run this toolkit type; the string is the server's own
   * sentence, and an EMPTY string is still a verdict (unavailable, no reason
   * given) — so this is tested against `undefined`, never for truthiness.
   */
  readonly unavailableReason?: string | undefined;
}

/** @public Rendered by `pages/toolkits/EditToolkit.tsx` as the Indexes tab panel. */
export function IndexesTab(props: IndexesTabProps): ReactNode {
  const { toolkitId, values, selectedIndexTools, chatUI, renderCredentialsSelect, unavailableReason } = props;

  /**
   * `IndexActions` reads exactly two fields off `editToolDetail`: `type`,
   * and an OPTIONAL pre-resolved `schema` it falls back to
   * `convertToolkitSchema(toolkitSchemas[type])` without
   * (`IndexActions.tsx:224-228`). Only `type` is forwarded: `pages/toolkits`'
   * own edit-detail carries a differently-shaped `schema` (its
   * `properties` is an open `Record<string, unknown>`, not this slice's
   * `ToolkitSchemaProperty` map), and handing it over would mean casting a
   * shape neither side actually agrees on. Letting the documented fallback
   * run instead resolves the schema from the SAME live type-schema query
   * everything else on this tab already uses.
   */
  const editToolDetail = useMemo<EditToolDetail>(
    () => (typeof values['type'] === 'string' ? { type: values['type'] } : {}),
    [values],
  );
  const { LLMModelSelector, ChatMessageList, ClearChatButton } = chatUI;

  const projectId = useSelectedProjectId();
  const [notice, setNotice] = useState<{ readonly severity: 'success' | 'error'; readonly message: string } | undefined>(undefined);
  const dismissNotice = useCallback(() => setNotice(undefined), []);

  const { data: llmModels } = useToolkitLlmModels(projectId === undefined ? undefined : String(projectId));
  const stopIndexingMutation = useStopIndexingItemMutation();

  const chatModels = useMemo(() => (llmModels?.models ?? []).map(toChatModel), [llmModels]);
  const defaultChatModel = useMemo(() => (llmModels?.defaultModel ? toChatModel(llmModels.defaultModel) : null), [llmModels]);

  const onSuccess = useCallback((message: string) => setNotice({ severity: 'success', message }), []);
  const onError = useCallback((message: string) => setNotice({ severity: 'error', message }), []);

  const stopIndexing = useCallback(
    async (input: { readonly projectId: string | undefined; readonly toolkitId: string | undefined; readonly indexName: string | undefined; readonly taskId: string | undefined }): Promise<void> => {
      if (input.projectId === undefined || input.toolkitId === undefined || input.indexName === undefined || input.taskId === undefined) return;
      await stopIndexingMutation.mutateAsync({
        projectId: input.projectId,
        toolkitId: input.toolkitId,
        indexName: input.indexName,
        taskId: input.taskId,
      });
    },
    [stopIndexingMutation],
  );

  /**
   * `useToolkitChat` with this file's eight extra params bound. `useMemo`,
   * not a module-level wrapper: the bound params change with the model list
   * and the notice handlers, and a stale closure here would run every index
   * against whatever model was loaded on first render (the closure-staleness
   * class of bug). Identity churn is harmless — React hooks key on CALL
   * ORDER, not on the identity of the function that owns the call, and
   * `IndexDetails` invokes this unconditionally, once, in a fixed position.
   */
  const boundUseToolkitChat = useMemo(
    () =>
      (params: UseToolkitChatParams): UseToolkitChatResult => {
        const onMcpAuthRequired = params.onMcpAuthRequired;
        // eslint-disable-next-line react-hooks/rules-of-hooks -- this IS the hook; `IndexDetails` calls the returned function in hook position (see the DI treaty in `IndexDetails.tsx`).
        const result = useToolkitChat({
          toolkitId: params.toolkitId,
          runTool: params.runTool ?? '',
          isValidForm: params.isValidForm,
          toolInputVariables: params.toolInputVariables,
          index: toChatIndex(params.index),
          traceNewIndex: params.traceNewIndex,
          refetchIndexesList: params.refetchIndexesList,
          cancelIndexingCallback: params.cancelIndexingCallback,
          values: params.values,
          modes: params.modes,
          onMcpAuthRequired: onMcpAuthRequired === undefined ? undefined : (message) => onMcpAuthRequired(message),
          modelList: chatModels,
          defaultModel: defaultChatModel,
          createConversation: createToolkitConversation,
          addParticipant: addToolkitConversationParticipant,
          stopIndexing,
          buildMessagePayload: buildToolkitMessagePayload,
          onSuccess,
          onError,
        });
        return {
          ...result,
          // Widened, not re-implemented: the slice's own contract types the
          // model as `unknown` (it has no opinion on the model shape); the
          // real hook types it as `ToolkitChatModel`. A `(m: ToolkitChatModel)
          // => void` is not assignable to a `(m: unknown) => void` under
          // `strictFunctionTypes`, so the two setters are re-wrapped here
          // rather than the contract loosened at either end.
          onSelectModel: (model: unknown) => result.onSelectModel(model as ToolkitChatModel),
          onSetLLMSettings: (settings: Record<string, unknown>) => result.onSetLLMSettings(settings),
        };
      },
    [chatModels, defaultChatModel, stopIndexing, onSuccess, onError],
  );

  const useSelectedToolSchema = useCallback(
    (params: { toolkitType: string; toolOptionType: string | null }) => {
      const available = values['available_mcp_tools'];
      // eslint-disable-next-line react-hooks/rules-of-hooks -- called in hook position by `IndexDetails`, same DI treaty as `boundUseToolkitChat` above.
      return useGetSelectedToolSchema({
        toolkitType: params.toolkitType,
        toolOptionType: params.toolOptionType,
        availableMcpTools: Array.isArray(available) ? (available as readonly McpToolOption[]) : undefined,
      });
    },
    [values],
  );

  const useToolkitSchemas = useCallback((params: { isMCP: boolean }) => {
    // eslint-disable-next-line react-hooks/rules-of-hooks -- called in hook position by `IndexDetails`, same DI treaty as `boundUseToolkitChat` above.
    return useGetCurrentToolkitSchemas({ isMCP: params.isMCP });
  }, []);

  if (unavailableReason !== undefined) {
    return (
      <Alert
        severity="info"
        variant="outlined"
        data-testid="indexes-unavailable"
        sx={{ margin: '1.5rem' }}
      >
        <AlertTitle>{t('features.toolkits.indexesTab.unavailableTitle', 'Indexing is not available for this toolkit type')}</AlertTitle>
        {/*
          * The server's sentence verbatim. It names the type and the reason
          * (a missing worker capability, an SDK import that failed), which a
          * generic line here could not, and it is written for a human — see
          * `type_catalogue.go`'s `unavailable_reason`. The fallback covers a
          * verdict that arrives without one.
          */}
        {unavailableReason === ''
          ? t('features.toolkits.indexesTab.unavailableGeneric', 'The worker that runs indexing jobs does not support this toolkit type.')
          : unavailableReason}
      </Alert>
    );
  }

  return (
    <>
      <IndexesContainer
        toolkitId={toolkitId}
        selectedIndexTools={selectedIndexTools}
        values={values}
        editToolDetail={editToolDetail}
        useToolkitChat={boundUseToolkitChat}
        useSelectedToolSchema={useSelectedToolSchema}
        useToolkitSchemas={useToolkitSchemas}
        ToolFormField={IndexToolFormField}
        LLMModelSelector={LLMModelSelector}
        ChatMessageList={ChatMessageList}
        ClearChatButton={ClearChatButton}
        renderCredentialsSelect={renderCredentialsSelect}
        onError={onError}
      />
      {/*
        * No global toast host exists in this app (the gap `IndexActions.tsx`
        * and `IndexesContainer.tsx` each disclose for their own swallowed
        * errors) — same local-Snackbar pattern `processes/chat/ui/
        * ChatConversationSidebar.tsx` established for exactly this reason.
        */}
      {notice !== undefined && (
        <Snackbar
          open
          autoHideDuration={6000}
          onClose={dismissNotice}
          anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        >
          <Alert
            severity={notice.severity}
            onClose={dismissNotice}
            variant="filled"
          >
            {notice.message}
          </Alert>
        </Snackbar>
      )}
    </>
  );
}
