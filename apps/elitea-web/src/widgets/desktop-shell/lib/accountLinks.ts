/**
 * The account menu's links out of the app, to the public
 * elitea-ng/elitea-platform repository: a prefilled bug report, and the
 * release notes of the running version.
 *
 * The repository is PUBLIC, so the report carries only the app version, the
 * platform and the language — never the deployment URL, the tenant or the
 * person's identity. The person reviews the issue in their browser before it
 * is filed.
 */
const REPOSITORY = 'https://github.com/elitea-ng/elitea-platform';

export interface BugReportContext {
  /** The app's version (`HostState.clientVersion`); absent outside the desktop app. */
  readonly version: string | undefined;
  readonly userAgent: string;
  readonly locale: string;
}

export function bugReportUrl({ version, userAgent, locale }: BugReportContext): string {
  const body = [
    '### What happened',
    '',
    '',
    '### What you expected',
    '',
    '',
    '### Steps to reproduce',
    '',
    '1. ',
    '',
    '### Environment',
    '',
    `- Elitea desktop: ${version ?? 'unknown'}`,
    `- Platform: ${userAgent}`,
    `- Language: ${locale}`,
    '',
    '<!-- This repository is public: leave out your deployment URL, tokens, customer data and other private details. Settings › Troubleshoot can show the app logs to attach. -->',
  ].join('\n');
  const query = new URLSearchParams({ title: '[desktop] ', labels: 'bug', body });
  return `${REPOSITORY}/issues/new?${query.toString()}`;
}

/**
 * The running release's notes. Released builds are stamped with the release
 * version (`publish.yml`, `desktop` job), whose tag is `v<version>`; a local
 * build keeps the committed `0.x` placeholder, which names no release, so it
 * gets the list of releases.
 */
export function whatsNewUrl(version: string | undefined): string {
  if (version === undefined || !/^[1-9]\d*\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)) return `${REPOSITORY}/releases`;
  return `${REPOSITORY}/releases/tag/v${version}`;
}
