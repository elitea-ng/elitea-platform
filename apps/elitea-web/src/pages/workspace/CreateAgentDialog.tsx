/**
 * "Create agent" in a desktop thread (ADR-0037): the builder's one question,
 * then one of two branches.
 *
 * - Local agent: an `AGENTS.md` in this folder. The dialog composes an
 *   ordinary message (`localAgentPrompt`) and sends it into this thread; the
 *   thread's agent writes the file with its own write tools, behind the
 *   approval card. No new tool or host command, and the D0 loader reads the
 *   file on the next turn.
 * - Elitea agent: the streamed "Build with AI" builder (`GenerateAgentModal`),
 *   saved through the reviewed create route. The session then lists it and
 *   picks it, so the next message runs it.
 *
 * Only a thread whose turns run on this computer offers the local branch; a
 * thread running in the cloud goes straight to the Elitea builder.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { initialBranch, isValidScope, localAgentPrompt, type AgentBranch } from '@elitea/client-core/agent-builder';

import { GenerateAgentModal } from '@/features/agents';
import type { ApplicationCreatedResponse } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

export interface CreateAgentDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: number;
  /** The thread runs on this computer, in a folder the agent can write to. */
  readonly localFolder: boolean;
  /** A message can go out now (an agent is picked and no turn is running). */
  readonly canSendLocal: boolean;
  /** Send the local-agent request into this thread. */
  readonly onLocal: (prompt: string) => void;
  readonly onCreated: (result: ApplicationCreatedResponse) => void;
}

function BranchChoice({ onPick }: { readonly onPick: (branch: AgentBranch) => void }): ReactNode {
  const options: { branch: AgentBranch; title: string; body: string }[] = [
    {
      branch: 'local',
      title: t('workspace.createAgent.localTitle', 'Local agent'),
      body: t('workspace.createAgent.localBody', 'An AGENTS.md in this folder. Every turn here follows it; commit it to share it.'),
    },
    {
      branch: 'elitea',
      title: t('workspace.createAgent.eliteaTitle', 'Elitea agent'),
      body: t('workspace.createAgent.eliteaBody', 'Generated with AI and saved to the project. It runs here, on the web and on mobile.'),
    },
  ];
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <Typography variant="bodyMedium">{t('workspace.createAgent.question', 'Which kind of agent?')}</Typography>
      {options.map((option) => (
        <ButtonBase
          key={option.branch}
          onClick={() => onPick(option.branch)}
          sx={(theme) => ({ display: 'block', textAlign: 'left', padding: 1.5, borderRadius: 1, border: `1px solid ${theme.vars.palette.divider}` })}
        >
          <Typography variant="labelMedium">{option.title}</Typography>
          <Typography variant="bodySmall" color="text.secondary" sx={{ display: 'block' }}>
            {option.body}
          </Typography>
        </ButtonBase>
      ))}
    </Box>
  );
}

interface LocalForm {
  request: string;
  scope: string;
  name: string;
}

const EMPTY_FORM: LocalForm = { request: '', scope: '', name: '' };

export function CreateAgentDialog(props: CreateAgentDialogProps): ReactNode {
  const { open, onClose, projectId, localFolder, canSendLocal, onLocal, onCreated } = props;
  const [branch, setBranch] = useState<AgentBranch | undefined>(() => initialBranch({ localFolder }));
  const [form, setForm] = useState<LocalForm>(EMPTY_FORM);
  const close = (): void => {
    setBranch(initialBranch({ localFolder }));
    setForm(EMPTY_FORM);
    onClose();
  };

  // A thread that moved to the cloud has no folder to write to any more.
  const shown = localFolder ? branch : 'elitea';
  if (open && shown === 'elitea') {
    return (
      <GenerateAgentModal
        open
        onClose={close}
        projectId={String(projectId)}
        onAgentCreated={(result) => {
          onCreated(result);
          close();
        }}
      />
    );
  }

  const scopeValid = form.scope.trim() === '' || isValidScope(form.scope);
  const ready = form.request.trim() !== '' && scopeValid && canSendLocal;
  const submit = (): void => {
    if (!ready) return;
    onLocal(localAgentPrompt({ request: form.request, scope: form.scope, name: form.name }));
    close();
  };

  const content =
    shown === 'local' ? (
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
        <TextField
          label={t('workspace.createAgent.request', 'What should the agent do?')}
          multiline
          minRows={4}
          value={form.request}
          onChange={(event) => setForm({ ...form, request: event.target.value })}
        />
        <TextField
          label={t('workspace.createAgent.scope', 'Folder')}
          placeholder={t('workspace.createAgent.scopePlaceholder', 'The whole folder (AGENTS.md at the root)')}
          value={form.scope}
          error={!scopeValid}
          helperText={
            scopeValid
              ? t('workspace.createAgent.scopeHelp', 'A subfolder such as apps/web gets its own AGENTS.md, read with the root one.')
              : t('workspace.createAgent.scopeInvalid', 'Use a folder inside this one, such as apps/web.')
          }
          onChange={(event) => setForm({ ...form, scope: event.target.value })}
        />
        <TextField label={t('workspace.createAgent.name', 'Name (optional)')} value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} />
        {!canSendLocal && (
          <Typography variant="bodySmall" color="text.secondary">
            {t('workspace.createAgent.cannotSend', 'Pick an agent for this thread, or wait for the running turn to finish.')}
          </Typography>
        )}
      </Box>
    ) : (
      <BranchChoice onPick={setBranch} />
    );

  const actions =
    shown === 'local' ? (
      <>
        <BaseBtn variant="secondary" onClick={() => setBranch(undefined)}>
          {t('workspace.createAgent.back', 'Back')}
        </BaseBtn>
        <BaseBtn variant="contained" color="primary" disabled={!ready} onClick={submit}>
          {t('workspace.createAgent.write', 'Ask the agent to write it')}
        </BaseBtn>
      </>
    ) : undefined;

  return <BaseModal open={open} title={t('workspace.createAgent.title', 'Create agent')} onClose={close} content={content} actions={{ node: actions }} />;
}
