/**
 * REST client for the outbound e-mail settings (gap G7).
 *
 * The relay this deployment sends invitations, moderation notices and test
 * messages through. It is what the admin Configuration page's "E-mail" section
 * is about, and it is NOT stored as plugin configuration: the SMTP password is
 * a credential, and the plugin-config value endpoints keep their values in
 * plaintext rows readable by everyone who can open that page. So the settings
 * have their own surface, and this module speaks to it.
 *
 * Wire contract: `services/elitea-main/internal/api/v2/admin/email.go`.
 *
 *   GET  /admin/email/administration
 *   PUT  /admin/email/administration
 *   POST /admin/email/test/administration
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as
 * `./adminIdentityProvidersApi.ts`, and reusing its failure readers rather than
 * restating them.
 *
 * ## Two documents, and both are needed
 *
 * A read returns the STORED layer (what an administrator typed here) and the
 * EFFECTIVE one (those rows laid over the deployment's environment defaults),
 * plus a per-field `sources` tag. A page that showed only one of them could not
 * tell an unset field from an inherited one, and the operator would retype a
 * value the chart already supplies — or, worse, read a blank host as "e-mail is
 * off" while the deployment sends perfectly well.
 *
 * ## The password is never in hand here
 *
 * No route returns it. A read says `password_set` and which layer supplied it.
 * This module holds a plaintext password only for the moment between the
 * operator typing one and the save that seals it.
 *
 * ## `password` is tri-state on save, and that is load-bearing
 *
 * Absent, empty string, and a value mean three different things — leave the
 * sealed password alone, clear it, and re-seal it. The form cannot echo the
 * current password (it never receives it), so a save that always sent the field
 * would erase the credential every time an operator corrected a host name, and
 * every message after that would be refused by the relay at AUTH.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

/** A mail relay is a deployment fact; there is no project-scoped view of it. */
const ADMIN_MODE = 'administration';

const EMAIL_URL = `/admin/email/${ADMIN_MODE}`;
const EMAIL_TEST_URL = `/admin/email/test/${ADMIN_MODE}`;

/**
 * The `managed_surface` value the server declares on the section this editor
 * owns. Exported so the page's registry keys on the SERVER's word rather than
 * on a section id this app chose to recognise.
 */
export const EMAIL_MANAGED_SURFACE = 'email';

/** How the SMTP session is secured. `tls` is the implicit mode on port 465. */
export type AdminEmailTlsMode = 'none' | 'starttls' | 'tls';

/** One LAYER of the settings. Every field may be blank: the layer under it may
 * supply what this one omits. */
export interface AdminEmailSettings {
  readonly host: string;
  readonly port: number;
  readonly username: string;
  readonly tls: string;
  readonly from: string;
  readonly reply_to: string;
  readonly public_base_url: string;
}

/** Which layer decided one field. */
export type AdminEmailSource = 'database' | 'environment' | 'unset';

/** What a read returns. It never carries a password. */
export interface AdminEmailState {
  /** What an administrator typed here. */
  readonly settings: AdminEmailSettings;
  /** The merged document the next message will actually use. */
  readonly effective: AdminEmailSettings;
  /** Per-field: which layer decided the effective value. */
  readonly sources: Readonly<Record<string, AdminEmailSource>>;
  /** A password is in force. The value is never returned. */
  readonly password_set: boolean;
  readonly password_source: AdminEmailSource;
  /** Whether a message can be submitted right now. */
  readonly configured: boolean;
  /** Why not, when `configured` is false. Names the field to set. */
  readonly reason?: string;
}

const EMPTY_SETTINGS: AdminEmailSettings = {
  host: '',
  port: 0,
  username: '',
  tls: '',
  from: '',
  reply_to: '',
  public_base_url: '',
};

/** What a read means when the server said nothing about a field. */
const DEFAULT_STATE: AdminEmailState = {
  settings: EMPTY_SETTINGS,
  effective: EMPTY_SETTINGS,
  sources: {},
  password_set: false,
  password_source: 'unset',
  configured: false,
};

/**
 * One query-key namespace, declared once.
 *
 * Every mutation invalidates `adminEmailKeys.all`. A key built ad hoc at a call
 * site would be a cache the writes never refresh — the read/write key-namespace
 * split that made saved data look absent in #132.
 */
const adminEmailKeys = {
  all: ['admin', 'email'] as const,
  state: () => ['admin', 'email', 'state'] as const,
};

/**
 * The server's document, with every field defaulted.
 *
 * A read that came back without `sources` must render as "nothing is set here",
 * never as a crash and never as a page with no tags at all — the second reads
 * to an operator as "this deployment has no environment defaults", which is a
 * different and wrong statement.
 */
function withDefaults(body: Partial<AdminEmailState> | undefined): AdminEmailState {
  // An explicitly-undefined key is DROPPED rather than spread over the
  // defaults: `{...defaults, ...{host: undefined}}` yields `undefined`, which
  // would put the crash back that the defaults exist to prevent.
  const stated = Object.fromEntries(
    Object.entries(body ?? {}).filter(([, value]) => value !== undefined),
  ) as Partial<AdminEmailState>;
  return { ...DEFAULT_STATE, ...stated };
}

/** `GET /admin/email/administration`. */
export function useAdminEmailSettings(): UseQueryResult<AdminEmailState, Error> {
  return useQuery({
    queryKey: adminEmailKeys.state(),
    queryFn: async (): Promise<AdminEmailState> => {
      // `eliteaFetch` resolves the transport envelope, not the body. Forgetting
      // to peel the body is #132's silent empty state.
      const body = unwrapBody(await eliteaFetch<unknown>(EMAIL_URL)) as Partial<AdminEmailState> | undefined;
      return withDefaults(body);
    },
  });
}

/** What the form collects. `password` absent ⇒ leave the sealed one alone. */
export interface AdminEmailDraft {
  readonly host: string;
  readonly port: number;
  readonly username: string;
  readonly tls: AdminEmailTlsMode | '';
  readonly from: string;
  readonly replyTo: string;
  readonly publicBaseURL: string;
  /** `undefined` ⇒ unchanged. `''` ⇒ clear it. A value ⇒ re-seal it. */
  readonly password: string | undefined;
}

/** `PUT /admin/email/administration`. */
export function useSaveAdminEmailSettings(): UseMutationResult<AdminEmailState, Error, AdminEmailDraft> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: AdminEmailDraft): Promise<AdminEmailState> => {
      const body: Record<string, unknown> = {
        host: draft.host,
        port: draft.port,
        username: draft.username,
        tls: draft.tls,
        from: draft.from,
        reply_to: draft.replyTo,
        public_base_url: draft.publicBaseURL,
      };
      // Sent ONLY when the operator changed it. See the tri-state note in this
      // module's header: an always-sent field would clear the credential on
      // every unrelated edit.
      if (draft.password !== undefined) {
        body['password'] = draft.password;
      }
      return unwrapBody(
        await eliteaFetch<unknown>(EMAIL_URL, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }),
      ) as AdminEmailState;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminEmailKeys.all }),
  });
}

/**
 * `POST /admin/email/test/administration`.
 *
 * The message goes through the same resolution as every other one, so this
 * verifies the SETTINGS rather than the process's environment: pressing it
 * after a save says whether what was just typed works.
 */
export function useSendAdminEmailTest(): UseMutationResult<void, Error, string> {
  return useMutation({
    mutationFn: async (to: string) => {
      await eliteaFetch<unknown>(EMAIL_TEST_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to }),
      });
    },
  });
}
