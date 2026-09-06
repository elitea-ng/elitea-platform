/**
 * The caller's own project id from `GET /social/author`, ASKED AGAIN until the
 * server names one.
 *
 * ## The defect this repairs
 *
 * On a first login the SPA sends TWO `/social/author` requests at boot, and
 * they do not get the same answer:
 *
 *   GET /api/v2/social/author/  ->  {"id":"7", …,"personal_project_id":"5"}
 *   GET /api/v2/social/author   ->  {"id":"7", …,"personal_project_id":""}
 *
 * Measured verbatim against a brand-new account on the standalone stack, both
 * answered inside the same second. The trailing-slash one is `app/session-
 * store.ts`'s boot probe; the other is this query, the one the shell reads.
 *
 * The server is not confused — it is provisioning. `GetAuthor`
 * (services/elitea-main/internal/api/v2/social/handler.go) creates the
 * personal project when the caller has none and waits up to
 * `defaultPersonalProjectWait` (3s) for the attempt IT started, then reports
 * the id. The second request finds the attempt already in flight
 * (`Ensurer.EnsureStarted` returns nil for a user another attempt owns), so it
 * has nothing to wait for and answers "" — correctly, for the instant it was
 * asked.
 *
 * Nothing then asked again. `GetAuthor` has no `staleTime` override, so the
 * shell held `personal_project_id: ""` for the life of the page, and every
 * piece of the shell that hangs off it stayed switched off: no project was
 * selected, the switcher read "Project: No projects", and the nav lost every
 * permission-gated row. A manual reload fixed it, which is the signature of a
 * value that was true when it was read and was never read again.
 *
 * ## Why a poll, and not a smarter trigger
 *
 * "" is not an error and not a terminal answer: it means "not yet", and the
 * only way to learn that it changed is to ask. This is the same contract the
 * onboarding screen already relies on — the server's own comment calls the
 * five-second SPA poll the thing that finishes the job when its bounded wait
 * runs out. The shell needs it too, because a first login does not necessarily
 * pass through onboarding: the boot probe's answer is what decides that, and
 * it is the request that WINS the race.
 *
 * It stops the moment the server names a project, which is the ordinary case
 * on every login after the first, so a normal page load pays nothing. It also
 * stops after `MAX_AUTHOR_POLLS` answers, so an account that will never have a
 * personal project (no membership anywhere) costs a bounded burst, not a
 * request every three seconds for as long as the tab is open.
 */
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';

/**
 * Three seconds: the same order as the server's own bounded wait, and inside
 * the patience of somebody who has just signed in for the first time.
 */
const AUTHOR_POLL_INTERVAL_MS = 3_000;

/** Ten answers — 30s of provisioning — after which asking again is not going to help. */
const MAX_AUTHOR_POLLS = 10;

/**
 * `useGetCurrentAuthor()`'s `.data` is the enveloped `{data, status, headers}`
 * shape `pages/chat/useChatPageData.ts`'s `currentAuthorOf` reads through
 * (`getCurrentAuthorResponse200`, `shared/api/generated/social/social.ts`) —
 * `eliteaFetch` throws on non-2xx (§3.6 unwrap contract), so the 401 branch is
 * declared but unreachable at this read site, same established precedent.
 *
 * `""` is normalised to `undefined` here, ONCE, because "" and "absent" are
 * the same fact — the server has no personal project to name — and every
 * caller that had to remember the difference is a caller that could forget it.
 */
function personalProjectIdOf(data: unknown): string | undefined {
  const id = (data as { readonly data?: SocialAuthorProfile } | undefined)?.data?.personal_project_id;
  return id === undefined || id === '' ? undefined : id;
}

/** The caller's personal project id, or `undefined` while the server names none. */
export function usePersonalProjectId(): string | undefined {
  const query = useGetCurrentAuthor({
    query: {
      refetchInterval: (authorQuery) => {
        if (personalProjectIdOf(authorQuery.state.data) !== undefined) return false;
        // `dataUpdateCount` counts the ANSWERS this query has taken, so the
        // bound holds whether the polls came from this observer or another.
        if (authorQuery.state.dataUpdateCount >= MAX_AUTHOR_POLLS) return false;
        return AUTHOR_POLL_INTERVAL_MS;
      },
    },
  });

  return personalProjectIdOf(query.data);
}
