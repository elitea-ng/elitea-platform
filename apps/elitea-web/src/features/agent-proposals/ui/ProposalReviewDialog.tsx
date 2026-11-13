/**
 * Review one agent proposal (ADR-0050 decision 5a): the ADR-0037 review form,
 * prefilled from the proposal's draft, with what the proposal adds.
 *
 * - The soul (display name and prose) the sub-agent ran with, editable.
 * - The proposed tools with their ADR-0041 category. They are not bound by
 *   Create: a person binds tools in the editor, where the gates apply.
 * - The memory the proposer suggested, each entry a checkbox; only the kept
 *   ones are seeded into the new agent.
 * - "Starts at Low tier": the create route's tier, never the proposer's.
 *
 * Create makes the agent through the ordinary create route and only then
 * records the proposal as created; Dismiss suppresses the same spec for 30
 * days. "Save locally" downloads AGENTS.md and SOUL.md and saves nothing.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentProposal } from '@/shared/api/generated/model';
import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { triggerBlobDownload } from '@/shared/lib/download';
import { MAX_DESCRIPTION_LENGTH, MAX_NAME_LENGTH } from '@/shared/lib/limits';
import { exceedsLimit } from '@/shared/lib/textLimits';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BaseModal } from '@/shared/ui/BaseModal';

import { agentsMarkdownOf, fileStemOf, reviewDraftOf, soulMarkdownOf, type ReviewDraft } from '../lib/proposalDraft';
import { AgentMadeError } from '../lib/proposalErrors';
import { useProposalDecisions, type CreatedAgent } from '../model/useAgentProposals';
import { ProposalToolList } from './ProposalToolList';

export interface ProposalReviewDialogProps {
  readonly projectId: string;
  /** The proposal under review; `undefined` closes the dialog. */
  readonly proposal: AgentProposal | undefined;
  readonly onClose: () => void;
  /** Called once the agent exists, before the dialog closes. */
  readonly onCreated?: ((created: CreatedAgent) => void) | undefined;
}

/** The runtime's sub-agent display name bound (agentproposal.MaxDisplayNameChars). */
const MAX_DISPLAY_NAME_LENGTH = 60;

/** Which of the review's name and description are past the entity limits (issue 1968); the inputs do not cut. */
function textOverLimits(review: { readonly name: string; readonly description: string }): { nameTooLong: boolean; descriptionTooLong: boolean; anyTooLong: boolean } {
  const nameTooLong = exceedsLimit(review.name, MAX_NAME_LENGTH);
  const descriptionTooLong = exceedsLimit(review.description, MAX_DESCRIPTION_LENGTH);
  return { nameTooLong, descriptionTooLong, anyTooLong: nameTooLong || descriptionTooLong };
}

export function ProposalReviewDialog({ projectId, proposal, onClose, onCreated }: ProposalReviewDialogProps): ReactNode {
  if (proposal === undefined) return null;
  return <ProposalReviewForm key={proposal.id} projectId={projectId} proposal={proposal} onClose={onClose} onCreated={onCreated} />;
}

function ProposalReviewForm({ projectId, proposal, onClose, onCreated }: ProposalReviewDialogProps & { readonly proposal: AgentProposal }): ReactNode {
  const { create, dismiss } = useProposalDecisions(projectId);
  const [review, setReview] = useState<ReviewDraft>(() => reviewDraftOf(proposal));
  const [kept, setKept] = useState<ReadonlySet<number>>(() => new Set(proposal.memory_seeds.map((_seed, index) => index)));
  const [error, setError] = useState<string | undefined>(undefined);
  // The agent an attempt created before recording the proposal failed: a
  // retry records this one instead of creating a second agent.
  const [madeId, setMadeId] = useState<number | undefined>(undefined);
  const busy = create.isPending || dismiss.isPending;
  const decided = proposal.status !== 'proposed';
  const nameMissing = review.name.trim() === '';
  const { nameTooLong, descriptionTooLong, anyTooLong } = textOverLimits(review);

  const field =
    (key: keyof Omit<ReviewDraft, 'conversationStarters'>) =>
    (event: ChangeEvent<HTMLInputElement>): void =>
      setReview((current) => ({ ...current, [key]: event.target.value }));

  const toggleSeed = (index: number, checked: boolean): void =>
    setKept((current) => {
      const next = new Set(current);
      if (checked) next.add(index);
      else next.delete(index);
      return next;
    });

  const submit = (): void => {
    if (nameMissing || anyTooLong || busy || decided) return;
    setError(undefined);
    create.mutate(
      { proposal, review, seeds: [...kept].sort((a, b) => a - b), existingApplicationId: madeId },
      {
        onSuccess: (created) => {
          onCreated?.(created);
          onClose();
        },
        onError: (cause) => {
          if (cause instanceof AgentMadeError) {
            setMadeId(cause.applicationId);
            setError(
              serverErrorMessage(
                cause.cause,
                t('features.agentProposals.review.recordFailed', 'The agent was created, but the proposal was not updated. Try again to finish.'),
              ),
            );
            return;
          }
          setError(serverErrorMessage(cause, t('features.agentProposals.review.createFailed', 'The agent was not created. Try again.')));
        },
      },
    );
  };

  const reject = (): void => {
    setError(undefined);
    dismiss.mutate(proposal.id, {
      onSuccess: onClose,
      onError: (cause) => setError(serverErrorMessage(cause, t('features.agentProposals.review.dismissFailed', 'The proposal was not dismissed. Try again.'))),
    });
  };

  const saveLocally = (): void => {
    const stem = fileStemOf(review.name);
    triggerBlobDownload(new Blob([agentsMarkdownOf(review, proposal)], { type: 'text/markdown' }), `${stem}.AGENTS.md`);
    if (review.soul.trim() !== '') triggerBlobDownload(new Blob([soulMarkdownOf(review)], { type: 'text/markdown' }), `${stem}.SOUL.md`);
  };

  return (
    <BaseModal
      open
      variant="complex"
      data-testid="agent-proposal-review"
      title={t('features.agentProposals.review.title', 'Review the proposed agent')}
      onClose={onClose}
      actions={{
        node: (
          <>
            <Button onClick={saveLocally} disabled={busy} data-testid="agent-proposal-save-locally">
              {t('features.agentProposals.review.saveLocally', 'Save locally as AGENTS.md / SOUL.md')}
            </Button>
            <BaseBtn variant="secondary" onClick={reject} disabled={busy || decided} data-testid="agent-proposal-dismiss">
              {t('features.agentProposals.review.dismiss', 'Dismiss')}
            </BaseBtn>
            <BaseBtn variant="contained" onClick={submit} disabled={busy || decided || nameMissing || anyTooLong} data-testid="agent-proposal-create">
              {t('features.agentProposals.review.create', 'Create agent')}
            </BaseBtn>
          </>
        ),
      }}
      content={
        <Box sx={formSx}>
          <Typography variant="bodyMedium" color="text.secondary" data-testid="agent-proposal-rationale">
            {t('features.agentProposals.review.proposedBy', '{{name}} proposes saving a sub-agent it ran: {{rationale}}', {
              name: proposal.proposer_name,
              rationale: proposal.rationale,
            })}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary" data-testid="agent-proposal-tier">
            {t('features.agentProposals.review.lowTier', 'The new agent starts at the Low risk tier. Nothing is saved until you press Create agent.')}
          </Typography>
          <TextField
            label={t('features.agentProposals.review.name', 'Name')}
            value={review.name}
            onChange={field('name')}
            error={nameMissing || nameTooLong}
            helperText={nameTooLong ? `Name must be ${MAX_NAME_LENGTH} characters or fewer` : undefined}
            slotProps={{ htmlInput: { 'data-testid': 'agent-proposal-name' } }}
          />
          <TextField
            label={t('features.agentProposals.review.description', 'Description')}
            value={review.description}
            onChange={field('description')}
            multiline
            minRows={2}
            error={descriptionTooLong}
            helperText={descriptionTooLong ? `Description must be ${MAX_DESCRIPTION_LENGTH} characters or fewer` : undefined}
            slotProps={{ htmlInput: { 'data-testid': 'agent-proposal-description' } }}
          />
          <TextField
            label={t('features.agentProposals.review.instructions', 'Instructions')}
            value={review.instructions}
            onChange={field('instructions')}
            multiline
            minRows={4}
            slotProps={{ htmlInput: { 'data-testid': 'agent-proposal-instructions' } }}
          />
          <TextField
            label={t('features.agentProposals.review.displayName', 'Display name')}
            value={review.displayName}
            onChange={field('displayName')}
            slotProps={{ htmlInput: { maxLength: MAX_DISPLAY_NAME_LENGTH, 'data-testid': 'agent-proposal-display-name' } }}
          />
          <TextField
            label={t('features.agentProposals.review.soul', 'Soul')}
            value={review.soul}
            onChange={field('soul')}
            multiline
            minRows={3}
            slotProps={{ htmlInput: { 'data-testid': 'agent-proposal-soul' } }}
          />
          <ProposalToolList tools={proposal.tool_categories} />
          {proposal.memory_seeds.length > 0 && (
            <Box sx={seedsSx} data-testid="agent-proposal-seeds">
              <Typography variant="labelMedium">{t('features.agentProposals.review.memory', 'Memory to start with')}</Typography>
              {proposal.memory_seeds.map((seed, index) => (
                <FormControlLabel
                  key={`${index}-${seed.content}`}
                  control={
                    <BaseCheckbox
                      checked={kept.has(index)}
                      onChange={(_event, checked) => toggleSeed(index, checked)}
                      data-testid={`agent-proposal-seed-${index}`}
                    />
                  }
                  label={`${seed.kind}: ${seed.content}`}
                />
              ))}
            </Box>
          )}
          {decided && (
            <Typography variant="bodySmall" color="text.secondary" data-testid="agent-proposal-decided">
              {t('features.agentProposals.review.decided', 'This proposal was already decided.')}
            </Typography>
          )}
          {error !== undefined && (
            <Typography role="alert" variant="bodySmall" color="error" data-testid="agent-proposal-error">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}

const formSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '32rem' };
const seedsSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem' };
