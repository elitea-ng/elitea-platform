/**
 * Hire a team (ADR-0051 amendment, issue 3137): one dialog with the mode, the
 * saved team's name and, per member, the connections it needs.
 *
 * Install reuses an agent this project already installed from the same member
 * version, so that member needs nothing; Fork makes new editable agents for
 * every member. Each member the hire makes binds its required toolkits, or
 * marks them "Disable for now", as a single agent hire does. The hire is one
 * transaction on the server: a refusal keeps nothing and names the member.
 */
import { useEffect, useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import { hireMarketplaceTeam } from '@/shared/api/generated/marketplace/marketplace';
import type {
  MarketplaceListingDetail,
  MarketplaceTeamDetail,
  MarketplaceTeamHireResult,
  MarketplaceTeamMember,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import { invalidateMarketplace } from '../invalidate';

import { BindingsStep, ModeStep } from './HireSteps';
import { applyChoice, type HireMode } from './hireModel';
import {
  membersStillUnbound,
  reusesMember,
  teamHireErrorMessage,
  teamHireRequest,
  unavailableMembers,
  type TeamChoices,
} from './teamHireModel';
import { useBindingTargets, type BindingOption } from './useBindingTargets';

export interface TeamHireDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string | undefined;
  readonly detail: MarketplaceListingDetail;
  readonly team: MarketplaceTeamDetail;
  readonly initialMode: HireMode;
}

function useTeamHire(props: TeamHireDialogProps) {
  const { open, projectId, detail, team, initialMode } = props;
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<HireMode>(initialMode);
  const [name, setName] = useState(detail.listing.name);
  const [choices, setChoices] = useState<TeamChoices>({});
  const hire = useMutation({
    mutationFn: async (): Promise<MarketplaceTeamHireResult> => {
      const response = await hireMarketplaceTeam(
        projectId ?? '',
        teamHireRequest(detail.listing.id, team, mode, name, choices),
      );
      return unwrapBody(response) as MarketplaceTeamHireResult;
    },
    onSuccess: () => invalidateMarketplace(queryClient),
  });
  const { reset } = hire;
  // Every open starts again, so a closed dialog never resumes with a half-made choice.
  useEffect(() => {
    if (!open) return;
    setMode(initialMode);
    setName(detail.listing.name);
    setChoices({});
    reset();
  }, [open, initialMode, detail.listing.name, reset]);
  const choose = (position: number, key: string, value: string): void => {
    setChoices((current) => ({ ...current, [position]: applyChoice(current[position] ?? {}, key, value) }));
    hire.reset();
  };
  const ready =
    name.trim() !== '' &&
    unavailableMembers(team).length === 0 &&
    membersStillUnbound(team, mode, choices).length === 0;
  return { mode, setMode, name, setName, choices, choose, hire, ready };
}

interface MemberSectionProps {
  readonly member: MarketplaceTeamMember;
  readonly mode: HireMode;
  readonly choices: TeamChoices;
  readonly optionsFor: (requirement: MarketplaceTeamMember['requirements']['items'][number]) => readonly BindingOption[];
  readonly onChoose: (position: number, key: string, value: string) => void;
}

/** One member: what it needs from the project, or why it needs nothing. */
function MemberSection({ member, mode, choices, optionsFor, onChoose }: MemberSectionProps): ReactNode {
  const headingId = `team-hire-member-${member.position}`;
  return (
    <Box component="section" aria-labelledby={headingId} sx={memberSx} data-testid={headingId}>
      <Typography id={headingId} component="h3" variant="labelMedium">
        {t('marketplace.teamHire.member', '{{position}}. {{name}} {{version}}', {
          position: member.position,
          name: member.name,
          version: member.version_name,
        })}
      </Typography>
      {!member.hireable && (
        <Alert severity="error">
          {t('marketplace.teamHire.unavailable', 'This agent version cannot be hired now, so the team cannot be hired.')}
        </Alert>
      )}
      {member.hireable && reusesMember(member, mode) && (
        <Typography variant="bodySmall" color="text.secondary" data-testid={`team-hire-reused-${member.position}`}>
          {t('marketplace.teamHire.reused', 'Uses the agent this project already installed from this version.')}
        </Typography>
      )}
      {member.hireable && !reusesMember(member, mode) && (
        <BindingsStep
          requirements={member.requirements}
          choices={choices[member.position] ?? {}}
          optionsFor={optionsFor}
          onChoose={(key, value) => onChoose(member.position, key, value)}
        />
      )}
    </Box>
  );
}

function HireDone({ result }: { readonly result: MarketplaceTeamHireResult }): ReactNode {
  const waiting = result.members.some((member) => member.awaiting_approval);
  const unbound = result.members.some((member) => member.unbound.length > 0);
  return (
    <Alert severity={waiting || unbound ? 'info' : 'success'} data-testid="team-hire-done">
      {t('marketplace.teamHire.done', 'Hired. The team {{name}} is in your Teams.', { name: result.name })}
      {waiting &&
        ` ${t('marketplace.teamHire.doneApproval', 'Some agents keep their tools off until a person approves their hire change request.')}`}
      {unbound &&
        ` ${t('marketplace.teamHire.doneUnbound', 'Some tools stay unavailable until you bind them on the agent page.')}`}
    </Alert>
  );
}

export function TeamHireDialog(props: TeamHireDialogProps): ReactNode {
  const { open, onClose, projectId, detail, team } = props;
  const state = useTeamHire(props);
  const targets = useBindingTargets(projectId, open);
  const result = state.hire.data;
  const actions =
    result === undefined ? (
      <>
        <BaseBtn variant="secondary" onClick={onClose}>
          {t('shared.ui.baseModal.cancel', 'Cancel')}
        </BaseBtn>
        <BaseBtn
          variant="contained"
          disabled={!state.ready || state.hire.isPending}
          onClick={() => state.hire.mutate()}
          data-testid="team-hire-confirm"
        >
          {t('marketplace.teamHire.confirm', 'Hire team')}
        </BaseBtn>
      </>
    ) : (
      <BaseBtn variant="contained" onClick={onClose} data-testid="team-hire-close">
        {t('marketplace.teamHire.close', 'Close')}
      </BaseBtn>
    );
  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={t('marketplace.teamHire.title', 'Hire the team {{name}}', { name: detail.listing.name })}
      data-testid="team-hire-dialog"
      actions={{ node: actions }}
      content={
        <Box sx={contentSx}>
          {result !== undefined ? (
            <HireDone result={result} />
          ) : (
            <>
              <ModeStep value={state.mode} onChange={state.setMode} />
              <TextField
                size="small"
                label={t('marketplace.teamHire.name', 'Team name')}
                value={state.name}
                onChange={(event) => state.setName(event.target.value)}
                slotProps={{ htmlInput: { maxLength: 128, 'data-testid': 'team-hire-name' } }}
              />
              {team.members.map((member) => (
                <MemberSection
                  key={member.position}
                  member={member}
                  mode={state.mode}
                  choices={state.choices}
                  optionsFor={targets.optionsFor}
                  onChoose={state.choose}
                />
              ))}
              {state.hire.isError && (
                <Alert severity="error" data-testid="team-hire-error">
                  {teamHireErrorMessage(state.hire.error)}
                </Alert>
              )}
            </>
          )}
        </Box>
      }
    />
  );
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: 2, minWidth: { xs: 0, sm: 480 } };

const memberSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: 1 };
