/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/
 * useChatSkillMention.hooks.js` — drives the "~" skill-mention dropdown and
 * highlight for the chat input. Mirrors the chat "/" mention stack
 * (`useSlashMention`) but for a single-level "~skill" reference. Sources the
 * skills attached to the conversation's active agent participant (the server
 * resolves a `~Name` of those against the version itself), then — as the
 * desktop composer's "Project skills" — the project's other skills, which the
 * send names by id (`projectSkillIdsFor`) and the server reads under the
 * caller's permissions.
 *
 * **No skill-suggestion LIST component is ported in this unit** — the
 * baseline's own file list for this hook has no accompanying UI component
 * (unlike the "/" system's `SlashSuggestionList`/`ToolItem`/`ToolList`);
 * this hook only owns the state machine (`filteredItems`/`highlightedIndex`/
 * `onSelectSkill`). A future composition-root unit (C6) renders the actual
 * dropdown — `SkillMentionItem`'s `{name, description}` shape is
 * deliberately compatible with `shared/ui/MentionToolList`'s `MentionTool`
 * type, so that shared component is a natural (not mandatory) fit.
 *
 * **`activeParticipantDetails` param dropped, `fallbackAppVersionId`
 * substituted (disclosed).** The baseline reads `activeParticipantDetails
 * ?.version_details?.id` as a fallback when `activeParticipant.entity_settings
 * .version_id` is unpopulated (its own comment: "e.g. the agent editor's
 * test chat"). `activeParticipantDetails` there is a whole fetched-details
 * object with no equivalent type anywhere in this app yet. Rather than
 * inventing a `ParticipantDetails` shape this port doesn't otherwise need,
 * the fallback is taken as a plain `fallbackAppVersionId?: string | number`
 * — the caller resolves it however it likes (e.g. from a details query it
 * already has) and passes just the one field this hook actually reads.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { RefObject } from 'react';

import { normaliseSkills } from '@/entities/skill';
import type { Skill, SkillWire } from '@/entities/skill';
import type { Participant } from '@/entities/participant';
import { useListApplicationSkills } from '@/shared/api/generated/skills/skills';
import type { SkillsList } from '@/shared/api/generated/model';

import { MentionPhase, SKILL_TRIGGER } from '../constants/mention.constants';
import type { MentionPhaseValue } from '../constants/mention.constants';
import { parseMentionRanges } from '../utils/instructionsMention.utils';
import { useProjectSkillList } from './useProjectSkillList';
import type { ProjectSkillEntry } from './useProjectSkillList';
import type { MentionableItem, MentionRange } from '../utils/instructionsMention.utils';
import type { ChatInputHandle } from '../chatInputHandle';

/**
 * A skill attached to the active agent participant's version, as surfaced to
 * the "~" dropdown. Deliberately compatible with `shared/ui/MentionToolList`'s
 * `MentionTool` (`{name, description?}`) — see this file's header. Not
 * exported beyond this module (knip: no outside consumer by name) — only
 * reachable via `ReturnType<typeof mentionHooks.useChatSkillMention>` once a
 * real cross-slice caller needs to name it directly.
 */
interface SkillMentionItem extends MentionableItem {
  readonly skillId: string;
  readonly description?: string | undefined;
  readonly isToolkit: false;
  /** One of the project's skills that the agent version does not carry: sent to the server by id (`payload.skills`), not resolved from the text. */
  readonly isProject: boolean;
}

/** A committed "~skill" reference found in the current input text. Not exported beyond this module — same rationale as `SkillMentionItem` above. */
interface CommittedSkillMention {
  readonly name: string;
}

/**
 * Baseline reads this value with `||`, not `??`
 * (`useChatSkillMention.hooks.js:38-39,44`) — a `version_id`/fallback of `0`
 * is treated as absent, same as `undefined`, so the fallback/`skip` guard
 * still engages. Returning `undefined` for `0` here (not just for
 * non-finite input) preserves that falsy-check semantics through the
 * `??`/`!== undefined` checks below.
 */
function toVersionIdNumber(value: string | number | undefined): number | undefined {
  if (value === undefined) return undefined;
  const parsed = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(parsed) && parsed !== 0 ? parsed : undefined;
}

function toSkillMentionItems(skills: readonly Skill[]): SkillMentionItem[] {
  return skills
    .map(
      (skill): SkillMentionItem => ({
        name: skill.name,
        skillId: skill.id,
        isToolkit: false,
        isProject: false,
        ...(skill.description !== undefined ? { description: skill.description } : {}),
      }),
    )
    .sort((a, b) => a.name.localeCompare(b.name));
}

interface DetectedMention {
  readonly anchor: number;
  readonly query: string;
}

/** `useChatSkillMention.hooks.js`'s own `detectMention` — a "~" trigger must be preceded by start-of-text or whitespace, and the fragment up to the cursor must be whitespace-free. */
function detectMention(text: string, cursorPos: number): DetectedMention | null {
  const upToCursor = text.slice(0, cursorPos);
  const anchor = upToCursor.lastIndexOf(SKILL_TRIGGER);
  if (anchor === -1) return null;
  const prevChar = anchor > 0 ? text[anchor - 1] : '';
  if (prevChar !== '' && prevChar !== undefined && !/\s/.test(prevChar)) return null;
  const fragment = upToCursor.slice(anchor + SKILL_TRIGGER.length);
  if (/\s/.test(fragment)) return null;
  return { anchor, query: fragment };
}

export interface UseChatSkillMentionParams {
  /** Imperative handle of whatever chat-textarea component renders the actual input — see `../chatInputHandle.ts`. */
  readonly chatInput: RefObject<ChatInputHandle | null>;
  /** The currently-selected conversation participant, or `null`/`undefined` if none. Skills are only sourced when this is an `'application'` participant. */
  readonly activeParticipant: Participant | null | undefined;
  /** Fallback app-version id when `activeParticipant.entitySettings.versionId` is unpopulated — see this file's header. */
  readonly fallbackAppVersionId?: string | number | undefined;
  /** Fallback project id when `activeParticipant.entityMeta.projectId` is unpopulated. */
  readonly projectId?: string | undefined;
}

/** The agent version's skills first, then the project's other skills (not named like one of the version's, ignoring case), each group alphabetical. */
function withProjectSkills(versionItems: readonly SkillMentionItem[], project: readonly ProjectSkillEntry[]): readonly SkillMentionItem[] {
  if (project.length === 0) return versionItems;
  const taken = new Set(versionItems.map((item) => item.name.trim().toLowerCase()));
  const extra = project
    .filter((skill) => !taken.has(skill.name.trim().toLowerCase()))
    .map((skill): SkillMentionItem => ({ name: skill.name, skillId: skill.id, isToolkit: false, isProject: true, ...(skill.description !== undefined ? { description: skill.description } : {}) }))
    .sort((a, b) => a.name.localeCompare(b.name));
  return [...versionItems, ...extra];
}

/** Whether the "~" menu is open on an agent: the project's skills are read then, not before. */
function readsProjectSkills(isAgent: boolean, phase: MentionPhaseValue): boolean {
  return isAgent && phase !== MentionPhase.Idle;
}

/** The ids of the project skills `text` mentions as `~Name`, each once, in text order. */
function mentionedProjectSkillIds(text: string, items: readonly SkillMentionItem[]): readonly string[] {
  const byName = new Map(items.map((item) => [item.name, item]));
  const ids: string[] = [];
  for (const range of parseMentionRanges(text, items, SKILL_TRIGGER)) {
    const item = byName.get(text.slice(range.start + 1, range.end));
    if (item?.isProject === true && !ids.includes(item.skillId)) ids.push(item.skillId);
  }
  return ids;
}

export interface UseChatSkillMentionResult {
  readonly skillPhase: MentionPhaseValue;
  readonly filteredItems: readonly SkillMentionItem[];
  readonly committedMentions: readonly CommittedSkillMention[];
  readonly highlightedIndex: number;
  readonly onSkillInputChange: (value: string) => void;
  readonly onSkillKeyDown: (event: { readonly key: string; preventDefault: () => void }) => void;
  readonly onSelectSkill: (item: SkillMentionItem) => void;
  readonly resetSkill: () => void;
  readonly skillHighlightRanges: readonly MentionRange[];
  /** The ids of the project skills `text` mentions (`~Name`), each once: what the send puts in `payload.skills`. Empty when it names none. */
  readonly projectSkillIdsFor: (text: string) => readonly string[];
}

export function useChatSkillMention({ chatInput, activeParticipant, fallbackAppVersionId, projectId }: UseChatSkillMentionParams): UseChatSkillMentionResult {
  const [inputContent, setInputContent] = useState('');
  const [phase, setPhase] = useState<MentionPhaseValue>(MentionPhase.Idle);
  const [itemQuery, setItemQuery] = useState('');
  const [highlightedIndex, setHighlightedIndex] = useState(0);
  const mentionAnchorRef = useRef<number | null>(null);

  const isAgent = activeParticipant?.entityName === 'application';
  // entitySettings.versionId is not always populated (e.g. the agent editor's
  // test chat), so fall back to the caller-supplied fallbackAppVersionId.
  const appVersionId = toVersionIdNumber(activeParticipant?.entitySettings?.versionId) ?? toVersionIdNumber(fallbackAppVersionId);
  const participantProjectId = activeParticipant?.entityMeta?.projectId ?? projectId;
  const skillsEnabled = isAgent && participantProjectId !== undefined && appVersionId !== undefined;

  const applicationSkillsQuery = useListApplicationSkills(participantProjectId ?? '', appVersionId ?? 0, { query: { enabled: skillsEnabled } });

  // The project's skills are read once the "~" menu is first opened (the hook
  // keeps the result after it closes, so a typed `~Name` still highlights).
  // They belong to the CONVERSATION's project, which is the one the send's
  // route reads them from, not to the agent's own (possibly public) project.
  const projectSkills = useProjectSkillList(projectId, readsProjectSkills(isAgent, phase));

  const versionItems = useMemo<readonly SkillMentionItem[]>(() => {
    // See `applicationParticipants.ts`'s header (this app's established
    // convention) for why `.data.data` is cast rather than narrowed — the
    // error-envelope arm is unreachable here (eliteaFetch throws instead).
    const wire = applicationSkillsQuery.data?.data as SkillsList | undefined;
    if (!wire) return [];
    // `wire.items` (the generated, snake_case `Skill` zod type) is structurally
    // the same shape as `entities/skill`'s hand-written `SkillWire` (verified
    // field-for-field against `skill.zod.ts`) — cast rather than re-declared,
    // matching this app's established "cast at the wire boundary" convention
    // (e.g. `entities/participant`'s `applicationParticipants.ts` header).
    return toSkillMentionItems(normaliseSkills(wire.items as unknown as readonly SkillWire[]));
  }, [applicationSkillsQuery.data]);

  const mentionableItems = useMemo(() => withProjectSkills(versionItems, projectSkills), [versionItems, projectSkills]);

  const resetSkill = useCallback(() => {
    mentionAnchorRef.current = null;
    setPhase(MentionPhase.Idle);
    setItemQuery('');
  }, []);

  const onSkillInputChange = useCallback(
    (value: string) => {
      setInputContent(value);
      if (!value) {
        resetSkill();
        return;
      }
      const cursorPos = chatInput.current?.getCursorPosition() ?? value.length;
      const detected = detectMention(value, cursorPos);
      if (!detected) {
        resetSkill();
        return;
      }
      mentionAnchorRef.current = detected.anchor;
      setPhase(MentionPhase.Items);
      setItemQuery(detected.query);
    },
    [chatInput, resetSkill],
  );

  const filteredItems = useMemo<readonly SkillMentionItem[]>(() => {
    if (!mentionableItems.length) return [];
    if (!itemQuery) return mentionableItems;
    return mentionableItems.filter((item) => item.name.toLowerCase().includes(itemQuery.toLowerCase()));
  }, [mentionableItems, itemQuery]);

  useEffect(() => {
    setHighlightedIndex(0);
  }, [phase, filteredItems]);

  const onSelectSkill = useCallback(
    (item: SkillMentionItem) => {
      const ref = chatInput.current;
      if (ref) {
        const content = ref.getInputContent() ?? inputContent;
        const anchor = mentionAnchorRef.current ?? content.length;
        const cursorPos = ref.getCursorPosition() ?? content.length;
        const replacement = SKILL_TRIGGER + item.name + ' ';
        ref.replaceRange(anchor, cursorPos, replacement);
        setInputContent(content.slice(0, anchor) + replacement + content.slice(cursorPos));
      }
      resetSkill();
    },
    [chatInput, inputContent, resetSkill],
  );

  const onSkillKeyDown = useCallback(
    (event: { readonly key: string; preventDefault: () => void }) => {
      if (phase !== MentionPhase.Items || filteredItems.length === 0) return;
      const { key } = event;
      if (key === 'ArrowDown') {
        event.preventDefault();
        setHighlightedIndex((prev) => (prev + 1) % filteredItems.length);
      } else if (key === 'ArrowUp') {
        event.preventDefault();
        setHighlightedIndex((prev) => (prev <= 0 ? filteredItems.length - 1 : prev - 1));
      } else if (key === 'Enter') {
        event.preventDefault();
        const item = filteredItems[highlightedIndex];
        if (item) onSelectSkill(item);
      } else if (key === 'Escape') {
        resetSkill();
      }
    },
    [phase, filteredItems, highlightedIndex, onSelectSkill, resetSkill],
  );

  const projectSkillIdsFor = useCallback((text: string): readonly string[] => mentionedProjectSkillIds(text, mentionableItems), [mentionableItems]);

  const skillHighlightRanges = useMemo(() => parseMentionRanges(inputContent, mentionableItems, SKILL_TRIGGER), [inputContent, mentionableItems]);

  const committedMentions = useMemo<readonly CommittedSkillMention[]>(
    () => skillHighlightRanges.map((range) => ({ name: inputContent.slice(range.start + 1, range.end) })),
    [skillHighlightRanges, inputContent],
  );

  return {
    skillPhase: phase,
    filteredItems,
    committedMentions,
    highlightedIndex,
    onSkillInputChange,
    onSkillKeyDown,
    onSelectSkill,
    resetSkill,
    skillHighlightRanges,
    projectSkillIdsFor,
  };
}
