/**
 * The account menu's version line: "Version 1.72.3 · Server 1.72". The
 * client part is always there (a web build without a release reads "dev");
 * the server part only once the discovery document has answered.
 */
export interface VersionLabels {
  readonly client: string;
  readonly server: string;
}

export function versionLine(client: string, server: string | undefined, labels: VersionLabels): string {
  const own = `${labels.client} ${client}`;
  return server === undefined ? own : `${own} · ${labels.server} ${server}`;
}
