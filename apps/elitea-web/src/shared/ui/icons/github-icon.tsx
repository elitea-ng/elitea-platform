/**
 * Ported from `apps/elitea-ui/src/components/Icons/GithubIcon.jsx` (`GitHubIcon`).
 *
 * One of three brand glyphs the toolkit-type catalogue shows that S2's icon
 * sweep missed: the baseline keeps them as inline JSX components under
 * `src/components/Icons/`, not as files under `src/assets/`, which is what that
 * sweep read. `fill` is `currentColor` here instead of the baseline's
 * `palette.icon.fill.primary` read, so the colour comes from the call site like
 * every other icon in this directory.
 */
export { default as GithubIcon } from './svg/github-icon.svg?react';
