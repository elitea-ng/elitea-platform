import { FORGES, type ForgeCredential, type ForgeType, listForgeTitles } from '@/api/queries/workspaceHandoff';

/**
 * The cloud thread's push and pull-request credential (ADR-0034 decision 10, amended 2026-10-26;
 * client contract 1.29), as the web and desktop pickers offer it: per forge, the project's
 * configurations (its own, then shared ones), then the owner's own (`private`). Only the
 * reference is ever sent; the token stays on the server and never reaches the sandbox.
 */

export interface CredentialChoice extends ForgeCredential {
  key: string;
}

const FORGE_NAMES: Record<ForgeType, string> = { github: 'GitHub', gitlab: 'GitLab' };

export const choiceKey = (credential: ForgeCredential): string => `${credential.type}:${credential.private ? 'private' : 'project'}:${credential.elitea_title}`;

export interface ForgeListing {
  type: ForgeType;
  project: { own: string[]; shared: string[] } | undefined;
  personal: { own: string[] } | undefined;
}

/** Each title once per forge and scope, in the web's order. */
export function credentialChoices(listings: readonly ForgeListing[]): CredentialChoice[] {
  const seen = new Set<string>();
  const out: CredentialChoice[] = [];
  for (const { type, project, personal } of listings) {
    const add = (title: string, isPrivate: boolean) => {
      const credential: ForgeCredential = { type, elitea_title: title, private: isPrivate };
      const key = choiceKey(credential);
      if (seen.has(key)) return;
      seen.add(key);
      out.push({ ...credential, key });
    };
    for (const title of [...(project?.own ?? []), ...(project?.shared ?? [])]) add(title, false);
    for (const title of personal?.own ?? []) add(title, true);
  }
  return out;
}

/** "work-gh (GitHub)", "mine (GitLab, yours)". */
export function credentialLabel(credential: ForgeCredential): string {
  const forge = FORGE_NAMES[credential.type];
  return credential.private ? `${credential.elitea_title} (${forge}, yours)` : `${credential.elitea_title} (${forge})`;
}

/** The notice's control: what the thread pushes through now. */
export function credentialButtonTitle(current: ForgeCredential | null | undefined): string {
  return current ? `Pushes and pull requests: ${credentialLabel(current)}` : 'Pushes and pull requests: choose a credential';
}

/** The project's listings (with shared ones), and the owner's personal project's when it is another one. */
export async function loadCredentialChoices(projectId: number, personalProjectId: number | null, signal?: AbortSignal): Promise<CredentialChoice[]> {
  const other = personalProjectId !== null && personalProjectId !== projectId ? personalProjectId : null;
  const listings = await Promise.all(
    FORGES.map(async (type): Promise<ForgeListing> => ({
      type,
      project: await listForgeTitles(projectId, type, true, undefined, signal),
      personal: other === null ? undefined : await listForgeTitles(other, type, false, undefined, signal),
    })),
  );
  return credentialChoices(listings);
}

const REFUSED: Record<number, string> = {
  403: 'Pushes and pull requests from the cloud are turned off for this project.',
  422: 'That is not a GitHub or GitLab credential of this project. Choose another.',
};

export function credentialFailure(status: number | undefined): string {
  return (status !== undefined && REFUSED[status]) || 'The choice could not be saved. Try again.';
}
