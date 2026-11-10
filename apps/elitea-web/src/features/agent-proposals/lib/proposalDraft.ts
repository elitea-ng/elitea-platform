/**
 * The proposal's draft (`AgentProposal.draft`, built by elitea-main's
 * `agentproposal.Draft`) read into the review form, the create request the
 * form sends, and the "Save locally" files.
 *
 * The draft is server-built JSON typed as an open record on the wire, so each
 * field is read defensively: a missing or mistyped field is the empty value,
 * never a crash in the review form.
 *
 * WHAT THE CREATE REQUEST CARRIES. Name, description, instructions, welcome
 * message, conversation starters and the soul. NOT the tools: a proposed
 * tool is a reference (`toolkit/tool`) into the proposer's run, and binding
 * it to a toolkit of this project is the person's own step in the editor,
 * where the tool categories are shown again. NOT a risk tier either: the
 * create route starts every agent at `low`, which is the tier the proposal
 * promises.
 */
import type { AgentProposal, ApplicationCreateRequest } from '@/shared/api/generated/model';
import { MAX_DESCRIPTION_LENGTH, MAX_NAME_LENGTH } from '@/shared/lib/limits';

/** The version name every create path uses for an agent's first version. */
const FIRST_VERSION_NAME = 'base';

export interface ReviewDraft {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly welcomeMessage: string;
  readonly conversationStarters: readonly string[];
  /** The soul's display name, when the sub-agent had one. */
  readonly displayName: string;
  /** The soul's prose, when the sub-agent had one. */
  readonly soul: string;
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

/** At most `max` characters (code points), as the create route counts them. */
function clamp(value: string, max: number): string {
  const characters = Array.from(value);
  return characters.length <= max ? value : characters.slice(0, max).join('').trim();
}

function strings(value: unknown): readonly string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string' && item.trim() !== '') : [];
}

export function reviewDraftOf(proposal: Pick<AgentProposal, 'draft'>): ReviewDraft {
  const draft = proposal.draft;
  const soul = draft['soul'] !== null && typeof draft['soul'] === 'object' ? (draft['soul'] as Record<string, unknown>) : {};
  return {
    // Cut to the create route's bounds: a proposal stored before the server
    // cut them would otherwise open with a value Create refuses.
    name: clamp(text(draft['name']), MAX_NAME_LENGTH),
    description: clamp(text(draft['description']), MAX_DESCRIPTION_LENGTH),
    instructions: text(draft['instructions']),
    welcomeMessage: text(draft['welcome_message']),
    conversationStarters: strings(draft['conversation_starters']),
    displayName: text(soul['display_name']),
    soul: text(soul['body']),
  };
}

/** The create route's body for the reviewed draft. */
export function createRequestOf(review: ReviewDraft): ApplicationCreateRequest {
  const soul =
    review.soul.trim() !== '' || review.displayName.trim() !== ''
      ? {
          schema: 'elitea.soul/v1' as const,
          ...(review.displayName.trim() !== '' ? { display_name: review.displayName.trim() } : {}),
          ...(review.soul.trim() !== '' ? { body: review.soul.trim() } : {}),
        }
      : undefined;
  return {
    name: review.name.trim(),
    description: review.description,
    type: 'interface',
    versions: [
      {
        name: FIRST_VERSION_NAME,
        instructions: review.instructions,
        welcome_message: review.welcomeMessage,
        conversation_starters: [...review.conversationStarters],
        variables: [],
        tags: [],
        // A fresh agent: the create route's own step limit, nothing toggled.
        meta: { step_limit: 25, internal_tools: [] },
        ...(soul === undefined ? {} : { soul }),
      },
    ],
  };
}

function section(title: string, body: string): string {
  return body.trim() === '' ? '' : `## ${title}\n\n${body.trim()}\n\n`;
}

/**
 * The AGENTS.md a person may keep locally: the agent's name, purpose,
 * instructions and its proposed tools with their categories. Plain Markdown,
 * written for a person or a local agent loop to read.
 */
export function agentsMarkdownOf(review: ReviewDraft, proposal: Pick<AgentProposal, 'tool_categories' | 'memory_seeds'>): string {
  const tools = proposal.tool_categories.map((tool) => `- \`${tool.key}\` (${tool.category})`).join('\n');
  const seeds = proposal.memory_seeds.map((seed) => `- ${seed.kind}: ${seed.content}`).join('\n');
  return (
    `# ${review.name.trim() === '' ? 'Agent' : review.name.trim()}\n\n` +
    section('Purpose', review.description) +
    section('Instructions', review.instructions) +
    section('Tools', tools) +
    section('Memory', seeds)
  ).trimEnd() + '\n';
}

/** The SOUL.md: the sub-agent's persona, as the soul document's prose. */
export function soulMarkdownOf(review: ReviewDraft): string {
  const title = review.displayName.trim() !== '' ? review.displayName.trim() : review.name.trim();
  return `# ${title === '' ? 'Soul' : title}\n\n${review.soul.trim()}\n`;
}

/** A file name from the agent's name: letters, digits and dashes. */
export function fileStemOf(name: string): string {
  const stem = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  return stem === '' ? 'agent' : stem.slice(0, 64);
}
