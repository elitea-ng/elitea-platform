import { type ReactNode, useMemo } from 'react';

import PersonOffOutlinedIcon from '@mui/icons-material/PersonOffOutlined';
import Avatar from '@mui/material/Avatar';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { authorDisplayName, isDeletedAuthor } from '@/entities/author';
import { combineSx } from '@/shared/ui/lib/combineSx';

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Components/
 * Applications/AuthorsButton.jsx` (a 6-line wrapper: `<AuthorsButton
 * entityType="applications" />`) INLINING the real, shared implementation
 * it wraps, `apps/elitea-ui/src/pages/Common/Components/AuthorsButton.jsx`
 * — that file is not itself in this sub-unit's owned list and has no
 * `entities`/`shared` home to import from without a forbidden
 * `pages/Common`-shaped sideways dependency, so per the mission preamble's
 * own guidance for this exact file ("A4 should get its own tiny local
 * copy"), the agents domain gets its own local copy too.
 *
 * DISCLOSED DEVIATIONS from the baseline:
 *  - No ambient form context. The baseline reads `useFormikContext().
 *    values.version_details` as its default when no `versions` prop is
 *    given. This app has no Formik dependency — see `../model/types.ts`'s
 *    module doc comment (cites `McpAuthStatusBadge.tsx`'s established
 *    "`values` is a required prop instead" convention, followed by every
 *    other `features/agents/ui/*` component in this slice). `versions` is
 *    a required prop here instead of formik-context-with-a-fallback.
 *  - No avatar IMAGE. The baseline's `VersionAuthorAvatar` renders
 *    `versionInfo.split('|')`'s `avatar` segment — sourced from a Redux
 *    author map that carries a URL. This app's real `Author` type
 *    (`entities/author`'s `Author`, mirroring `internal/domain/
 *    applications/types.go:5-9`) has exactly THREE fields — `id`, `email`,
 *    `name` — no avatar URL exists anywhere in the modeled wire shape.
 *    Renders an initials `Avatar` instead (real, not invented — MUI's
 *    documented "monogram" fallback pattern).
 *  - No internal navigation. The baseline's `useNavigateToAuthorPublicPage`
 *    builds a `/user-public/:tab?author_id=...&author_name=...` URL with
 *    react-router `state`-based breadcrumbs. This app's real
 *    `/_shell/user-public/$tab` route (`src/routes/_shell/user-public/
 *    $tab.tsx`) currently declares `validateSearch: pickParams('statuses')`
 *    ONLY — no `author_id`/`author_name` search param is wired yet, and
 *    react-router's arbitrary `history.state` passthrough has no TanStack
 *    equivalent (same gap `features/agents/lib/useIsFromApplication.ts`
 *    already discloses). Takes an `onSelectAuthor` callback instead of
 *    guessing a not-yet-real route contract; a caller with the real
 *    contract wires its own navigation from there.
 */
export interface AuthorsButtonAuthor {
  readonly id: string;
  readonly name: string;
  readonly email: string;
}

export interface AuthorsButtonVersion {
  readonly author?: AuthorsButtonAuthor | undefined;
}

/** @public */
export interface AuthorsButtonProps {
  versions: readonly AuthorsButtonVersion[];
  onSelectAuthor?: (author: AuthorsButtonAuthor) => void;
  sx?: SxProps<Theme>;
}

/** Old app's `deduplicateVersionByAuthor` (`common/utils.jsx:233-244`), adapted to work on real `Author` objects instead of a `"name|avatar|id"` composite string. */
export function dedupeAuthors(versions: readonly AuthorsButtonVersion[]): AuthorsButtonAuthor[] {
  const seen = new Map<string, AuthorsButtonAuthor>();
  for (const version of versions) {
    const author = version.author;
    if (!author) continue;
    const key = `${author.name}|${author.id}`;
    if (!seen.has(key)) seen.set(key, author);
  }
  return [...seen.values()];
}

/** First letter of `name`, upper-cased — MUI's documented monogram-avatar fallback shape. */
export function authorInitial(name: string | undefined): string {
  const trimmed = (name ?? '').trim();
  return trimmed === '' ? '?' : trimmed.charAt(0).toUpperCase();
}

export function AuthorsButton({ versions, onSelectAuthor, sx }: AuthorsButtonProps): ReactNode {
  const authors = useMemo(() => dedupeAuthors(versions), [versions]);

  return (
    <>
      {authors.map((author) => (
        <Tooltip
          key={`${author.id}-${author.name}`}
          title={authorDisplayName(author)}
          placement="top"
        >
          <Avatar
            onClick={onSelectAuthor ? () => onSelectAuthor(author) : undefined}
            sx={combineSx(avatarSx(Boolean(onSelectAuthor)), sx)}
          >
            {isDeletedAuthor(author)
              ? <PersonOffOutlinedIcon fontSize="small" data-testid="deleted-author-avatar" />
              : authorInitial(author.name)}
          </Avatar>
        </Tooltip>
      ))}
    </>
  );
}

function avatarSx(clickable: boolean): SxProps<Theme> {
  return { width: 28, height: 28, cursor: clickable ? 'pointer' : 'default' };
}
