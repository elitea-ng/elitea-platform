/**
 * The Skills section of the conversation panel (ADR-0050 amendment 1, A1.3).
 *
 * It sits APART from Participants on purpose. A skill is used by whichever agent
 * or model answers, so it is not a participant: no avatar, no `#` address, no
 * voice. It lists the skills pinned to the conversation, each with who pinned it
 * and an "untrusted" badge when its text came from another project and this
 * project has not trusted it (ADR-0051 amendment 1, A1.6), and lets a person who
 * may write to the conversation add one of the project's skills or remove one.
 *
 * The server holds the list and the bound (five, shared with the skills one
 * message invokes); the section only shows what it answered and says why a
 * request was refused.
 */
import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';

import AddIcon from '@mui/icons-material/Add';
import CloseIcon from '@mui/icons-material/Close';

import { useNavigate } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';

import {
  CONVERSATION_SKILL_LIMIT,
  useConversationSkillMutations,
  useConversationSkills,
  type ConversationSkillPin,
} from '@/entities/conversation-skill';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { listSkills } from '@/shared/api/generated/skills/skills';
import { unwrapListPage } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

export interface ConversationSkillsSectionProps {
  readonly projectId: string | undefined;
  /** The saved conversation's id; a conversation still being composed has none, and shows no section. */
  readonly conversationId: string | undefined;
  /** The caller may not write to the conversation (a read-only view): the section lists, and changes nothing. */
  readonly readOnly?: boolean;
  /** The name of a person of the conversation by user id, so a row can say who pinned it. */
  readonly personName?: (userId: number) => string | undefined;
}

interface SkillRow {
  readonly id?: unknown;
  readonly name?: unknown;
}

/** The project's skills the add menu offers: one page of the project's own list, by name. */
function useProjectSkillOptions(projectId: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ['conversation-skills-options', projectId ?? ''],
    queryFn: async (): Promise<readonly { readonly id: number; readonly name: string }[]> =>
      unwrapListPage<SkillRow>(await listSkills(projectId ?? '', { page: 1, page_size: 100 }), 'conversationSkillOptions')
        .rows.flatMap((row) =>
          typeof row.id === 'number' && typeof row.name === 'string' && row.name.trim() !== ''
            ? [{ id: row.id, name: row.name }]
            : [],
        )
        .sort((left, right) => left.name.localeCompare(right.name)),
    enabled: enabled && projectId !== undefined && projectId !== '',
    staleTime: 30_000,
  });
}

/** Why a pin or unpin was refused, in the person's words; the server's own reason for the bound. */
function refusalText(error: unknown): string {
  const status = error instanceof EliteaApiError && error.failure.kind === 'http' ? error.failure.status : undefined;
  if (status === 409) {
    return t('chat.skills.limit', 'A conversation holds at most five skills. Remove one to add another.');
  }
  if (status === 404) {
    return t('chat.skills.unavailable', 'That skill is not available to you.');
  }
  return t('chat.skills.failed', 'The skills could not be changed. Try again.');
}

function pinnedByText(pin: ConversationSkillPin, currentUserId: string | undefined, personName: ConversationSkillsSectionProps['personName']): string {
  if (currentUserId !== undefined && String(pin.pinned_by) === String(currentUserId)) {
    return t('chat.skills.pinnedByYou', 'Pinned by you');
  }
  const name = personName?.(pin.pinned_by);
  return name === undefined || name === ''
    ? t('chat.skills.pinnedByOther', 'Pinned by a teammate')
    : t('chat.skills.pinnedBy', 'Pinned by {{name}}', { name });
}

function PinRow({
  pin,
  readOnly,
  removing,
  onRemove,
  pinnedBy,
}: {
  readonly pin: ConversationSkillPin;
  readonly readOnly: boolean;
  readonly removing: boolean;
  readonly onRemove: (pin: ConversationSkillPin) => void;
  readonly pinnedBy: string;
}): ReactNode {
  const name = pin.name === '' ? t('chat.skills.removed', 'A skill that is no longer available') : pin.name;
  return (
    <Box component="li" sx={rowSx} data-testid={`conversation-skill-${pin.id}`}>
      <Box sx={{ minWidth: 0, flex: 1 }}>
        <Typography variant="bodySmall" component="p" sx={nameSx}>
          {name}
        </Typography>
        <Box sx={metaSx}>
          {pin.kind === 'listing' && (
            <Typography variant="labelSmall" color="text.secondary">
              {t('chat.skills.fromMarketplace', 'Marketplace {{version}}', { version: pin.version_name ?? '' })}
            </Typography>
          )}
          <Typography variant="labelSmall" color="text.secondary" data-testid={`conversation-skill-pinned-by-${pin.id}`}>
            {pinnedBy}
          </Typography>
          {pin.trust.state === 'untrusted' && (
            <Chip
              size="small"
              variant="outlined"
              color="warning"
              label={t('chat.skills.untrusted', 'Untrusted')}
              title={t(
                'chat.skills.untrustedHint',
                'Written in another project. The model gets its text as quoted data until this project trusts it.',
              )}
              data-testid={`conversation-skill-untrusted-${pin.id}`}
            />
          )}
        </Box>
      </Box>
      {!readOnly && (
        <IconButton
          size="small"
          disabled={removing}
          onClick={() => onRemove(pin)}
          aria-label={t('chat.skills.remove', 'Remove {{name}} from this conversation', { name })}
          data-testid={`conversation-skill-remove-${pin.id}`}
        >
          <CloseIcon fontSize="small" />
        </IconButton>
      )}
    </Box>
  );
}

export function ConversationSkillsSection({ projectId, conversationId, readOnly = false, personName }: ConversationSkillsSectionProps): ReactNode {
  const currentUserId = (useGetCurrentAuthor().data?.data as { readonly id?: string } | undefined)?.id;
  const navigate = useNavigate();
  const list = useConversationSkills(projectId, conversationId);
  const { pin, unpin } = useConversationSkillMutations(projectId, conversationId);
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const options = useProjectSkillOptions(projectId, anchor !== null);
  const pins = useMemo(() => list.data ?? [], [list.data]);
  const full = pins.length >= CONVERSATION_SKILL_LIMIT;
  const pinned = useMemo(() => new Set(pins.flatMap((entry) => (entry.skill_id === undefined ? [] : [entry.skill_id]))), [pins]);

  // A conversation still being composed has nowhere to hold a pin.
  if (conversationId === undefined || conversationId === '') return null;

  const add = (skillId: number): void => {
    setAnchor(null);
    setRefusal(null);
    pin.mutate({ skill_id: skillId }, { onError: (error) => setRefusal(refusalText(error)) });
  };
  const remove = (entry: ConversationSkillPin): void => {
    setRefusal(null);
    unpin.mutate(entry.id, { onError: (error) => setRefusal(refusalText(error)) });
  };
  const addable = (options.data ?? []).filter((skill) => !pinned.has(skill.id));

  return (
    <Box component="section" sx={sectionSx} data-testid="conversation-skills" aria-label={t('chat.skills.title', 'Skills')}>
      <Box sx={headerSx}>
        <Typography variant="labelMedium" component="h3" color="text.secondary">
          {t('chat.skills.heading', 'Skills ({{count}})', { count: pins.length })}
        </Typography>
        {!readOnly && (
          <IconButton
            size="small"
            disabled={full}
            onClick={(event) => setAnchor(event.currentTarget)}
            aria-label={
              full
                ? t('chat.skills.addFull', 'Add a skill (this conversation holds five)')
                : t('chat.skills.add', 'Add a skill to this conversation')
            }
            data-testid="conversation-skills-add"
          >
            <AddIcon fontSize="small" />
          </IconButton>
        )}
      </Box>
      <Typography variant="labelSmall" color="text.secondary" component="p" sx={{ mb: 0.5 }}>
        {t('chat.skills.hint', 'Applied to every reply here, whichever agent or model answers.')}
      </Typography>
      {list.isError && (
        <Alert severity="error" data-testid="conversation-skills-error">
          {t('chat.skills.loadFailed', 'The skills of this conversation could not be loaded.')}
        </Alert>
      )}
      {refusal !== null && (
        <Alert severity="warning" onClose={() => setRefusal(null)} data-testid="conversation-skills-refusal">
          {refusal}
        </Alert>
      )}
      {!list.isError && pins.length === 0 && !list.isPending && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="conversation-skills-empty">
          {t('chat.skills.empty', 'No skills pinned.')}
        </Typography>
      )}
      <Box component="ul" sx={listSx}>
        {pins.map((entry) => (
          <PinRow
            key={entry.id}
            pin={entry}
            readOnly={readOnly}
            removing={unpin.isPending}
            onRemove={remove}
            pinnedBy={pinnedByText(entry, currentUserId, personName)}
          />
        ))}
      </Box>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)} data-testid="conversation-skills-menu">
        {options.isPending && (
          <MenuItem disabled>{t('chat.skills.loading', 'Loading the project skills…')}</MenuItem>
        )}
        {options.isError && <MenuItem disabled>{t('chat.skills.optionsFailed', 'The project skills could not be loaded.')}</MenuItem>}
        {options.isSuccess && addable.length === 0 && (
          <MenuItem disabled>{t('chat.skills.noneToAdd', 'Every project skill is pinned already, or the project has none.')}</MenuItem>
        )}
        {addable.map((skill) => (
          <MenuItem key={skill.id} onClick={() => add(skill.id)} data-testid={`conversation-skills-option-${skill.id}`}>
            {skill.name}
          </MenuItem>
        ))}
        <MenuItem
          onClick={() => {
            setAnchor(null);
            void navigate({ to: '/marketplace', search: { kind: 'skills' } });
          }}
        >
          {t('chat.skills.browse', 'Find more skills in the Marketplace')}
        </MenuItem>
      </Menu>
    </Box>
  );
}

const sectionSx = { width: '100%', padding: '0.5rem 0' } as const;
const headerSx = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' } as const;
const listSx = { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 0.5 } as const;
const rowSx = { display: 'flex', alignItems: 'center', gap: 0.5 } as const;
const nameSx = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', margin: 0 } as const;
const metaSx = { display: 'flex', alignItems: 'center', gap: 0.5, flexWrap: 'wrap' } as const;
