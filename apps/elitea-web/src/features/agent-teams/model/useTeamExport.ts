/**
 * Export on a team row (issue 3010): reads each member's markdown export at
 * its team version, zips them with `team.yaml` and saves the zip.
 */
import { useCallback, useState } from 'react';

import type { AgentTeam } from '@/shared/api/generated/model';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { fetchMarkdownExport, triggerBlobDownload } from '@/shared/lib/download';

import { buildTeamZip, teamZipName } from '../lib/teamExport';

export interface TeamExport {
  /** The id of the team being exported, if any. */
  readonly exportingId: number | undefined;
  readonly error: string | undefined;
  readonly exportTeam: (team: AgentTeam) => Promise<void>;
}

export function useTeamExport(): TeamExport {
  const [exportingId, setExportingId] = useState<number>();
  const [error, setError] = useState<string>();

  const exportTeam = useCallback(async (team: AgentTeam) => {
    const config = getConfig();
    if (config.status !== 'ok') return;
    setExportingId(team.id);
    setError(undefined);
    try {
      const markdown: string[] = [];
      for (const member of team.members) {
        const result = await fetchMarkdownExport({
          baseUrl: config.config.vite_server_url,
          projectId: String(member.project_id),
          applicationId: String(member.application_id),
          followVersionIds: [String(member.version_id)],
        });
        if (!result.ok) {
          setError(
            t('features.agentTeams.exportMemberFailed', 'The team could not be exported: the agent {{name}} could not be read.', {
              name: member.name === '' ? String(member.application_id) : member.name,
            }),
          );
          return;
        }
        markdown.push(result.text);
      }
      triggerBlobDownload(await buildTeamZip(team, markdown), teamZipName(team));
    } finally {
      setExportingId(undefined);
    }
  }, []);

  return { exportingId, error, exportTeam };
}
