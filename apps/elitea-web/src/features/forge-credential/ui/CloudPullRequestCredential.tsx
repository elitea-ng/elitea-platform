/**
 * The thread's push and pull-request credential (ADR-0034 decision 10,
 * amended 2026-10-26; contract 1.29): which GitHub or GitLab configuration
 * the cloud agent's `git_push` and `open_pull_request` write through. The
 * owner chooses it here: a configuration of the project (or a shared one), or
 * one of the owner's own (personal project, `private`). Only the REFERENCE is
 * sent, never a secret; Main redeems it for the thread's running cloud turn
 * only, and the token never reaches the sandbox.
 *
 * Each push and pull request is still asked about (an approval card) unless
 * the platform and project let them run without one. The chat page's cloud
 * notice and the desktop's cloud-thread banner both show it.
 */
import { useState } from 'react';

import { useMutation, useQuery } from '@tanstack/react-query';

import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import Typography from '@mui/material/Typography';

import { setWorkspaceHandoffPullRequestCredential } from '@/shared/api/generated/chat/chat';
import { listConfigurations } from '@/shared/api/generated/configurations/configurations';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { SocialAuthorProfile, WorkspaceForgeCredential } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { i18n, t } from '@/shared/i18n';

import catalogue from '../i18n/en.forgeCredential.json';

// These strings ride with the chunk that renders the picker (the chat page's
// notice, the desktop's cloud-thread banner), not the initial catalogue
// (scripts/i18n-backfill.mjs, ROUTE_CATALOGUES). Deep merge, no overwrite.
i18n.addResourceBundle('en', 'translation', catalogue, true, false);

/** One choice: the reference, and where it lives. */
export interface CredentialChoice extends WorkspaceForgeCredential {
  readonly key: string;
}

/** A configuration type a cloud thread pushes through. */
type WorkspaceForgeCredentialType = WorkspaceForgeCredential['type'];

/** The forges a cloud thread pushes to, in the order they are listed. */
const FORGES: readonly WorkspaceForgeCredentialType[] = ['github', 'gitlab'];
const FORGE_NAMES: Record<WorkspaceForgeCredentialType, string> = { github: 'GitHub', gitlab: 'GitLab' };
const NONE = '';

const choiceKey = (credential: WorkspaceForgeCredential): string => `${credential.type}:${credential.private ? 'private' : 'project'}:${credential.elitea_title}`;

interface ListedRow {
  readonly elitea_title?: string | null | undefined;
}

interface ListedPage {
  readonly items?: readonly ListedRow[] | undefined;
  readonly shared?: { readonly items?: readonly ListedRow[] | undefined } | undefined;
}

/** One forge's listings: the project's (with shared ones) and the owner's own. */
export interface ForgeListing {
  readonly type: WorkspaceForgeCredentialType;
  readonly project: ListedPage | undefined;
  readonly personal: ListedPage | undefined;
}

function titles(rows: readonly ListedRow[] | undefined): string[] {
  return (rows ?? []).map((row) => row.elitea_title ?? '').filter((title) => title !== '');
}

/** Per forge: the project's configurations (its own, then shared ones), then the owner's own; each title once per forge and scope. */
export function credentialChoices(listings: readonly ForgeListing[]): CredentialChoice[] {
  const seen = new Set<string>();
  const out: CredentialChoice[] = [];
  for (const { type, project, personal } of listings) {
    const add = (elitea_title: string, isPrivate: boolean): void => {
      const credential: WorkspaceForgeCredential = { type, elitea_title, private: isPrivate };
      const key = choiceKey(credential);
      if (seen.has(key)) return;
      seen.add(key);
      out.push({ ...credential, key });
    };
    for (const title of [...titles(project?.items), ...titles(project?.shared?.items)]) add(title, false);
    for (const title of titles(personal?.items)) add(title, true);
  }
  return out;
}

/** The owner's personal project (the chat page has already read `/social/author`, so this is its cache). */
function usePersonalProjectId(open: boolean): string | undefined {
  const author = useGetCurrentAuthor({ query: { enabled: open } });
  return (author.data as { readonly data?: SocialAuthorProfile } | undefined)?.data?.personal_project_id;
}

/** A listing that did not answer 200 is a failure, never an empty list: an empty list would hide the current choice. */
async function listForge(projectId: string, type: WorkspaceForgeCredentialType, includeShared: boolean): Promise<ListedPage> {
  const response = await listConfigurations(projectId, { type, include_shared: includeShared, limit: 200 });
  if (response.status !== 200) throw new Error(`listing ${type} configurations answered ${response.status}`);
  return response.data;
}

async function listAll(projectId: string, personalId: string | undefined): Promise<ForgeListing[]> {
  return Promise.all(
    FORGES.map(async (type) => ({
      type,
      project: await listForge(projectId, type, true),
      personal: personalId === undefined ? undefined : await listForge(personalId, type, false),
    })),
  );
}

interface Choices {
  readonly choices: CredentialChoice[];
  readonly loading: boolean;
  /** The listing failed: nothing can be saved, so nothing is cleared. */
  readonly failed: boolean;
  readonly retry: () => void;
}

function useChoices(projectId: string, open: boolean): Choices {
  const personalId = usePersonalProjectId(open);
  const otherPersonal = personalId !== undefined && personalId !== '' && personalId !== projectId ? personalId : undefined;
  const listings = useQuery({
    queryKey: ['pull-request-credentials', projectId, otherPersonal ?? ''],
    queryFn: () => listAll(projectId, otherPersonal),
    enabled: open,
  });
  return { choices: credentialChoices(listings.data ?? []), loading: listings.isPending, failed: listings.isError, retry: () => void listings.refetch() };
}

/** What Save sends for `selected`: `undefined` when nothing changes (the current choice kept, listed or not), `null` only for an explicit None. */
export function savedCredential(selected: string, current: WorkspaceForgeCredential | null | undefined, choices: readonly CredentialChoice[]): WorkspaceForgeCredential | null | undefined {
  if (selected === NONE) return current ? null : undefined;
  if (current && selected === choiceKey(current)) return undefined;
  const choice = choices.find((entry) => entry.key === selected);
  return choice ? { type: choice.type, elitea_title: choice.elitea_title, private: choice.private } : undefined;
}

function failureMessage(error: unknown): string {
  const status = error instanceof EliteaApiError && error.failure.kind === 'http' ? error.failure.status : 0;
  if (status === 403) return t('features.forgeCredential.disabled', 'Pushes and pull requests from the cloud are turned off for this project.');
  if (status === 422) return t('features.forgeCredential.invalid', 'That is not a GitHub or GitLab credential of this project. Choose another.');
  return t('features.forgeCredential.failed', 'The choice could not be saved. Try again.');
}

const choiceLabel = (choice: WorkspaceForgeCredential): string => {
  const forge = FORGE_NAMES[choice.type];
  return choice.private
    ? t('features.forgeCredential.private', '{{title}} ({{forge}}, yours)', { title: choice.elitea_title, forge })
    : t('features.forgeCredential.project', '{{title}} ({{forge}})', { title: choice.elitea_title, forge });
};

interface ChoiceListProps extends Choices {
  /** The current choice when the listing does not show it. */
  readonly unlisted: WorkspaceForgeCredential | undefined;
  readonly selected: string;
  readonly onSelect: (key: string) => void;
}

/** The dialog's body: loading, a failed listing (nothing can be saved), or the choices. */
function ChoiceList({ loading, failed, retry, choices, unlisted, selected, onSelect }: ChoiceListProps): React.JSX.Element {
  return (
    <>
      {loading ? (
        <Typography variant="bodySmall">{t('features.forgeCredential.loading', 'Loading GitHub and GitLab credentials…')}</Typography>
      ) : failed ? (
        <>
          <Typography variant="bodySmall" color="error" role="alert" data-testid="cloud-pr-credential-list-failed">
            {t('features.forgeCredential.listFailed', 'The GitHub and GitLab credentials could not be listed. Nothing was changed.')}
          </Typography>
          <Button size="small" onClick={retry} data-testid="cloud-pr-credential-retry">
            {t('features.forgeCredential.retry', 'Try again')}
          </Button>
        </>
      ) : (
        <RadioGroup value={selected} onChange={(event) => onSelect(event.target.value)} data-testid="cloud-pr-credential-choices">
          <FormControlLabel value={NONE} control={<Radio />} label={t('features.forgeCredential.none', 'None')} />
          {unlisted && (
            <FormControlLabel
              value={choiceKey(unlisted)}
              control={<Radio />}
              data-testid="cloud-pr-credential-unlisted"
              label={t('features.forgeCredential.unlisted', '{{label}} (current, not listed)', { label: choiceLabel(unlisted) })}
            />
          )}
          {choices.map((choice) => (
            <FormControlLabel key={choice.key} value={choice.key} control={<Radio />} label={choiceLabel(choice)} />
          ))}
        </RadioGroup>
      )}
      {!loading && !failed && choices.length === 0 && (
        <Typography variant="bodySmall">{t('features.forgeCredential.empty', 'There is no GitHub or GitLab credential yet. Add one in Credentials, then choose it here.')}</Typography>
      )}
    </>
  );
}

export interface CloudPullRequestCredentialProps {
  readonly projectId: string;
  readonly conversationUuid: string;
  /** The thread's current choice, as the handoff state reads it. */
  readonly current: WorkspaceForgeCredential | null | undefined;
  /** Saved: the caller re-reads the thread's handoff state. */
  readonly onSaved: () => void;
  readonly onError: (message: string) => void;
}

/** "Pushes and pull requests: <title>" for the owner, and the dialog that chooses it. */
export function CloudPullRequestCredential({ projectId, conversationUuid, current, onSaved, onError }: CloudPullRequestCredentialProps): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState(current ? choiceKey(current) : NONE);
  const { choices, loading, failed, retry } = useChoices(projectId, open);
  // The current choice stays selectable (and is what Save keeps) even when the listing does not show it.
  const unlisted = current && !choices.some((entry) => entry.key === choiceKey(current)) ? current : undefined;
  const save = useMutation({
    mutationFn: (credential: WorkspaceForgeCredential | null) => setWorkspaceHandoffPullRequestCredential(projectId, conversationUuid, { credential }),
    onSuccess: () => {
      setOpen(false);
      onSaved();
    },
    onError: (error) => onError(failureMessage(error)),
  });
  const choose = (): void => {
    const credential = savedCredential(selected, current, choices);
    if (credential === undefined) setOpen(false);
    else save.mutate(credential);
  };
  const label = current
    ? t('features.forgeCredential.button', 'Pushes and pull requests: {{title}}', { title: choiceLabel(current) })
    : t('features.forgeCredential.buttonUnset', 'Pushes and pull requests: choose a credential');
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
        <DialogTitle id="cloud-pr-title">{t('features.forgeCredential.title', 'Pushes and pull requests from the cloud')}</DialogTitle>
        <DialogContent>
          <Typography variant="bodyMedium" sx={{ marginBottom: 1 }}>
            {t(
              'features.forgeCredential.body',
              "The cloud agent can push this thread's commits to a new branch of the folder's GitHub or GitLab repository and open a pull request (a merge request on GitLab), through the credential you choose. Nothing existing is overwritten. The credential stays on the server; you are asked before each push unless your project allows them without asking.",
            )}
          </Typography>
          <ChoiceList loading={loading} failed={failed} retry={retry} choices={choices} unlisted={unlisted} selected={selected} onSelect={setSelected} />
        </DialogContent>
        <DialogActions>
          <Button disabled={save.isPending} onClick={() => setOpen(false)}>
            {t('features.forgeCredential.cancel', 'Cancel')}
          </Button>
          <Button variant="contained" disabled={save.isPending || loading || failed} onClick={choose} data-testid="cloud-pr-credential-save">
            {t('features.forgeCredential.save', 'Save')}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
