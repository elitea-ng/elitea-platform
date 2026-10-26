/**
 * The thread's pull-request credential (ADR-0034 decision 10, amended
 * 2026-10-26; contract 1.29): which GitHub configuration the cloud agent's
 * `open_pull_request` writes through. The owner chooses it here: a GitHub
 * configuration of the project (or a shared one), or one of the owner's own
 * (personal project, `private`). Only the REFERENCE is sent, never a secret;
 * Main redeems it for the thread's running cloud turn only, and the token
 * never reaches the sandbox.
 *
 * Each pull request is still asked about in the chat (an approval card)
 * unless the platform and project let pull requests open without one.
 */
import { useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import Typography from '@mui/material/Typography';

import { handoffInfoKey } from '@/features/workspace-handoff';
import { setWorkspaceHandoffPullRequestCredential } from '@/shared/api/generated/chat/chat';
import { listConfigurations } from '@/shared/api/generated/configurations/configurations';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { SocialAuthorProfile, WorkspaceForgeCredential } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { t } from '@/shared/i18n';

/** One choice: the reference, and where it lives. */
export interface CredentialChoice extends WorkspaceForgeCredential {
  readonly key: string;
}

const NONE = '';

const choiceKey = (credential: WorkspaceForgeCredential): string => `${credential.private ? 'private' : 'project'}:${credential.elitea_title}`;

interface ListedRow {
  readonly elitea_title?: string | null | undefined;
}

interface ListedPage {
  readonly items?: readonly ListedRow[] | undefined;
  readonly shared?: { readonly items?: readonly ListedRow[] | undefined } | undefined;
}

function titles(rows: readonly ListedRow[] | undefined): string[] {
  return (rows ?? []).map((row) => row.elitea_title ?? '').filter((title) => title !== '');
}

/** The project's GitHub configurations (its own, then shared ones), then the owner's own; each title once per scope. */
export function credentialChoices(project: ListedPage | undefined, personal: ListedPage | undefined): CredentialChoice[] {
  const seen = new Set<string>();
  const out: CredentialChoice[] = [];
  const add = (elitea_title: string, isPrivate: boolean): void => {
    const credential = { elitea_title, private: isPrivate };
    const key = choiceKey(credential);
    if (seen.has(key)) return;
    seen.add(key);
    out.push({ ...credential, key });
  };
  for (const title of [...titles(project?.items), ...titles(project?.shared?.items)]) add(title, false);
  for (const title of titles(personal?.items)) add(title, true);
  return out;
}

/** The owner's personal project (the chat page has already read `/social/author`, so this is its cache). */
function usePersonalProjectId(open: boolean): string | undefined {
  const author = useGetCurrentAuthor({ query: { enabled: open } });
  return (author.data as { readonly data?: SocialAuthorProfile } | undefined)?.data?.personal_project_id;
}

async function listGitHub(projectId: string, includeShared: boolean): Promise<ListedPage> {
  const response = await listConfigurations(projectId, { type: 'github', include_shared: includeShared, limit: 200 });
  return response.status === 200 ? response.data : {};
}

function useChoices(projectId: string, open: boolean): { readonly choices: CredentialChoice[]; readonly loading: boolean } {
  const personalId = usePersonalProjectId(open);
  const otherPersonal = personalId !== undefined && personalId !== '' && personalId !== projectId ? personalId : undefined;
  const project = useQuery({ queryKey: ['pull-request-credentials', projectId], queryFn: () => listGitHub(projectId, true), enabled: open });
  const personal = useQuery({
    queryKey: ['pull-request-credentials', otherPersonal ?? ''],
    queryFn: () => listGitHub(otherPersonal ?? '', false),
    enabled: open && otherPersonal !== undefined,
  });
  return { choices: credentialChoices(project.data, personal.data), loading: project.isPending || (otherPersonal !== undefined && personal.isPending) };
}

function failureMessage(error: unknown): string {
  const status = error instanceof EliteaApiError && error.failure.kind === 'http' ? error.failure.status : 0;
  if (status === 403) return t('pages.chat.cloud.pr.disabled', 'Pull requests from the cloud are turned off for this project.');
  if (status === 422) return t('pages.chat.cloud.pr.invalid', 'That is not a GitHub credential of this project. Choose another.');
  return t('pages.chat.cloud.pr.failed', 'The choice could not be saved. Try again.');
}

export interface CloudPullRequestCredentialProps {
  readonly projectId: string;
  readonly conversationUuid: string;
  /** The thread's current choice, as the handoff state reads it. */
  readonly current: WorkspaceForgeCredential | null | undefined;
  readonly onError: (message: string) => void;
}

/** "Pull requests: <title>" for the owner, and the dialog that chooses it. */
export default function CloudPullRequestCredential({ projectId, conversationUuid, current, onError }: CloudPullRequestCredentialProps): React.JSX.Element {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState(current ? choiceKey(current) : NONE);
  const { choices, loading } = useChoices(projectId, open);
  const save = useMutation({
    mutationFn: (credential: WorkspaceForgeCredential | null) => setWorkspaceHandoffPullRequestCredential(projectId, conversationUuid, { credential }),
    onSuccess: () => {
      setOpen(false);
      void queryClient.invalidateQueries({ queryKey: handoffInfoKey(projectId, conversationUuid) });
    },
    onError: (error) => onError(failureMessage(error)),
  });
  const choose = (): void => {
    const choice = choices.find((entry) => entry.key === selected);
    save.mutate(choice ? { elitea_title: choice.elitea_title, private: choice.private } : null);
  };
  const label = current
    ? t('pages.chat.cloud.pr.button', 'Pull requests: {{title}}', { title: current.elitea_title })
    : t('pages.chat.cloud.pr.buttonUnset', 'Pull requests: choose a GitHub credential');
  return (
    <>
      <Button
        size="small"
        color="inherit"
        data-testid="cloud-pr-credential-button"
        onClick={() => {
          setSelected(current ? choiceKey(current) : NONE);
          setOpen(true);
        }}
      >
        {label}
      </Button>
      <Dialog open={open} onClose={save.isPending ? undefined : () => setOpen(false)} aria-labelledby="cloud-pr-title">
        <DialogTitle id="cloud-pr-title">{t('pages.chat.cloud.pr.title', 'Pull requests from the cloud')}</DialogTitle>
        <DialogContent>
          <Typography variant="bodyMedium" sx={{ marginBottom: 1 }}>
            {t(
              'pages.chat.cloud.pr.body',
              "The cloud agent can push a new branch with this thread's changes to the folder's GitHub repository and open a pull request, through the credential you choose. The credential stays on the server; you are asked before each pull request unless your project allows them without asking.",
            )}
          </Typography>
          {loading ? (
            <Typography variant="bodySmall">{t('pages.chat.cloud.pr.loading', 'Loading GitHub credentials…')}</Typography>
          ) : (
            <RadioGroup value={selected} onChange={(event) => setSelected(event.target.value)} data-testid="cloud-pr-credential-choices">
              <FormControlLabel value={NONE} control={<Radio />} label={t('pages.chat.cloud.pr.none', 'None')} />
              {choices.map((choice) => (
                <FormControlLabel
                  key={choice.key}
                  value={choice.key}
                  control={<Radio />}
                  label={choice.private ? t('pages.chat.cloud.pr.private', '{{title}} (yours)', { title: choice.elitea_title }) : choice.elitea_title}
                />
              ))}
            </RadioGroup>
          )}
          {!loading && choices.length === 0 && (
            <Typography variant="bodySmall">{t('pages.chat.cloud.pr.empty', 'There is no GitHub credential yet. Add one in Credentials, then choose it here.')}</Typography>
          )}
        </DialogContent>
        <DialogActions>
          <Button disabled={save.isPending} onClick={() => setOpen(false)}>
            {t('pages.chat.cloud.pr.cancel', 'Cancel')}
          </Button>
          <Button variant="contained" disabled={save.isPending || loading} onClick={choose} data-testid="cloud-pr-credential-save">
            {t('pages.chat.cloud.pr.save', 'Save')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
