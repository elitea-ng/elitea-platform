/**
 * The two provider calls one chat turn makes, as plain async functions.
 *
 * NOT the generated hooks, and the distinction is not stylistic. `orval.config.ts`
 * sets `override.query.useQuery: true`, so EVERY generated operation becomes a
 * query hook — `useInvokeDeepWikiTool` included. Calling that would fire the
 * POST as a query: on mount, again on refocus, and again on reconnect. A
 * question would be asked several times and billed several times, and the
 * screen would look fine. `wikiChatApi.test.ts` asserts this module imports the
 * function and not the hook.
 *
 * That is the house convention rather than a DeepWiki defect — every feature
 * hand-wraps `eliteaFetch` in its own mutation, and
 * `features/settings/api/ai-configuration/api.ts` is the pattern.
 */
import {
  getDeepWikiInvocation,
  invokeDeepWikiTool,
} from '@/shared/api/generated/deepwiki/deepwiki';
import { unwrapBody } from '@/shared/api/unwrap';
import { invocationIdFrom } from '@/entities/provider-run';
import type { ChatInvocationPoll, ChatInvokeInput } from '@/features/wiki-chat';

/** What the drawer knows about the toolkit it is asking. */
export interface WikiChatTarget {
  readonly projectId: number;
  readonly toolkitId: number;
  readonly toolkitName: string;
  readonly toolkitType: string;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly repoIdentifierOverride?: string | undefined;
  readonly analysisKeyOverride?: string | undefined;
  /**
   * The wiki version the reader has OPEN. It pins an attachment: the provider
   * resolves `context_paths` against this version's manifest and refuses a
   * page that is not published there, so a question asked against the version
   * on screen cannot be answered from a newer one that landed mid-read.
   */
  readonly wikiVersionId?: string | undefined;
  /**
   * Wiki page IDS attached to the next question. Never page text: the bodies
   * are resolved server-side from this project's own artifacts, which is what
   * keeps the browser from being able to choose what the server reads.
   */
  readonly contextPaths?: readonly string[] | undefined;
}

/**
 * The one setting the request cannot be built without.
 *
 * The legacy drawer threw here with this sentence, and the throw is the
 * behaviour: an invocation sent with no model is accepted by the facade and
 * fails inside the provider, where the user is told nothing useful.
 */
export function requireLlmModel(settings: Readonly<Record<string, unknown>>): string {
  // `llm_model` is the platform's key — the facade reads it, the generation
  // panel sends it, the seed writes it. The `toolkit_configuration_` prefix
  // is the legacy drawer's spelling, kept for a toolkit saved by the old app.
  const model = settings['llm_model'] ?? settings['toolkit_configuration_llm_model'];
  if (typeof model !== 'string' || model === '') {
    throw new Error('Toolkit settings missing llm_model. Configure it in toolkit settings first.');
  }
  return model;
}

/** The provider's invocation envelope for one question. */
export function buildInvokeRequest(target: WikiChatTarget, input: ChatInvokeInput) {
  const llmModel = requireLlmModel(target.settings);
  const maxTokens = target.settings['max_tokens'] ?? target.settings['toolkit_configuration_max_tokens'];

  return {
    configuration: { parameters: { ...target.settings } },
    parameters: {
      question: input.question,
      chat_history: input.history,
      // The overrides are OMITTED rather than sent empty: the provider's merge
      // rule takes a tool argument over a configuration value only when it is
      // truthy, so an empty string would be ignored anyway and a null would
      // widen the envelope for nothing.
      ...(target.repoIdentifierOverride ? { repo_identifier_override: target.repoIdentifierOverride } : {}),
      ...(target.analysisKeyOverride ? { analysis_key_override: target.analysisKeyOverride } : {}),
      // Both keys or neither. The provider refuses `context_paths` without a
      // version pin, so sending the selection alone would turn every attached
      // question into an error the reader cannot act on.
      ...(target.contextPaths && target.contextPaths.length > 0 && target.wikiVersionId
        ? {
            context_paths: [...target.contextPaths],
            context_wiki_version_id: target.wikiVersionId,
          }
        : {}),
      ...(input.capability === 'research'
        ? { research_type: 'general', enable_subagents: true }
        : {}),
      llm_model: llmModel,
      llm_settings: { max_tokens: maxTokens ?? 4096, model_name: llmModel },
      stream_id: input.streamId,
      message_id: input.messageId,
    },
  };
}

/**
 * The two headers that tell elitea-main which conversation a question belongs
 * to, so it can file the turn it is about to record.
 *
 * HEADERS AND NOT BODY FIELDS, and the reason is on the other side of the
 * hop: the body is an SPI envelope that travels to the provider almost
 * verbatim, so a key put there for the platform's own bookkeeping would be
 * forwarded to a service that has no idea what it means. The facade reads
 * these two and DELETES them before forwarding (its `WrapInvoke`).
 *
 * The toolkit is sent because the body cannot supply it: `Wikis` names a code
 * toolkit, `wikis_query` names a Wikis toolkit and `wiki_query` names nothing
 * at all, so there is no one field that means "the wiki I am looking at".
 */
function wikiChatHeaders(
  target: WikiChatTarget,
  conversationKey: string,
): Record<string, string> {
  return {
    'X-Elitea-Wiki-Chat': conversationKey,
    'X-Elitea-Wiki-Toolkit': String(target.toolkitId),
  };
}

/**
 * Start one invocation and return its id.
 *
 * `unwrapBody` because `eliteaFetch` resolves the transport ENVELOPE, not the
 * body: reading `invocation_id` off the envelope yields undefined on a 200, and
 * the drawer then polls `undefined` for ever with the spinner still turning
 * (issue #132's shape).
 */
export async function startWikiChat(
  target: WikiChatTarget,
  input: ChatInvokeInput,
  conversationKey?: string,
): Promise<string> {
  const response = await invokeDeepWikiTool(
    target.projectId,
    target.toolkitName,
    input.toolName,
    buildInvokeRequest(target, input),
    conversationKey ? { headers: wikiChatHeaders(target, conversationKey) } : undefined,
  );
  return invocationIdFrom(
    unwrapBody(response),
    'The provider accepted the question but returned no invocation to follow.',
  );
}

/** Poll one invocation. */
export async function pollWikiChat(
  target: WikiChatTarget,
  toolName: string,
  invocationId: string,
): Promise<ChatInvocationPoll | undefined> {
  const response = await getDeepWikiInvocation(
    target.projectId,
    target.toolkitName,
    toolName,
    invocationId,
  );
  return unwrapBody(response) as ChatInvocationPoll | undefined;
}
