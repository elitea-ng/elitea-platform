/**
 * The consent metadata of one saved MCP toolkit (FU-250). A fan-out sign-in
 * card names only its toolkit: Main reads the endpoint from storage, checks
 * the caller, and discovers the metadata through its guarded client. The
 * card's own server URL is never sent.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { McpAuthorizationChallenge } from '@/shared/api/generated/model';
import { getMcpAuthorizationChallenge } from '@/shared/api/generated/toolkits/toolkits';
import { t } from '@/shared/i18n';

export type McpAuthorizationChallengeOutcome =
  | { readonly kind: 'challenge'; readonly challenge: McpAuthorizationChallenge }
  | { readonly kind: 'refused'; readonly code: string; readonly message: string };

const REFUSALS: Readonly<Record<string, () => string>> = {
  toolkit_not_found: () => t('mcps.authorizationChallenge.notFound', 'This toolkit is not available to you, so it cannot be signed in to here.'),
  unsupported_toolkit_type: () => t('mcps.authorizationChallenge.unsupported', 'This toolkit does not support sign-in from a run.'),
  invalid_toolkit_resource: () => t('mcps.authorizationChallenge.invalidResource', "This toolkit's server address cannot be used for sign-in."),
  authorization_discovery_timeout: () => t('mcps.authorizationChallenge.timeout', 'The MCP server did not answer in time. Try again, or Skip.'),
  authorization_discovery_failed: () => t('mcps.authorizationChallenge.discoveryFailed', "The MCP server's sign-in details could not be read. Try again, or Skip."),
  authorization_resource_mismatch: () => t('mcps.authorizationChallenge.discoveryFailed', "The MCP server's sign-in details could not be read. Try again, or Skip."),
};

const unavailable = () => t('mcps.authorizationChallenge.unavailable', 'Sign-in is not available right now. Try again, or Skip.');

function refusalCode(error: unknown): string {
  if (!(error instanceof EliteaApiError)) return 'unavailable';
  const { failure } = error;
  if (failure.kind === 'auth') return 'authentication_required';
  if (failure.kind !== 'http' || typeof failure.body !== 'object' || failure.body === null) return 'unavailable';
  const code = (failure.body as { error?: unknown }).error;
  return typeof code === 'string' && code.length <= 64 ? code : 'unavailable';
}

export async function readMcpAuthorizationChallenge(projectId: string, toolkitId: number): Promise<McpAuthorizationChallengeOutcome> {
  try {
    const response = await getMcpAuthorizationChallenge(projectId, toolkitId);
    // Only the contract's fields reach the dialog; a missing one is refused.
    const parsed = McpAuthorizationChallenge.safeParse(response.data);
    if (!parsed.success) return { kind: 'refused', code: 'unavailable', message: unavailable() };
    return { kind: 'challenge', challenge: parsed.data };
  } catch (error) {
    const code = refusalCode(error);
    return { kind: 'refused', code, message: (REFUSALS[code] ?? unavailable)() };
  }
}
