import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vitest';

import { server } from '@/test/setup';

import { useFolderFilter } from '../lib/useFolderFilter';

import { BASE, PROJECT, renderInRouter, serveFolders } from './harness';

function Probe({ projectId }: { readonly projectId: string | undefined }): React.ReactElement {
  const filter = useFolderFilter(projectId, 'skill');
  return <div data-testid="probe">{`${String(filter.active)}|${String(filter.folderId)}|${String(filter.includes(11))}|${String(filter.includes(12))}`}</div>;
}

const folder = { id: '1', name: 'A', position: 1, item_ids: [11] };

describe('useFolderFilter', () => {
  it('narrows to the folder once the folders are known', async () => {
    serveFolders('skill', [folder]);
    renderInRouter(<Probe projectId={PROJECT} />, '/skills/all?entity_folder=1');
    await waitFor(() => expect(screen.getByTestId('probe')).toHaveTextContent('true|1|true|false'));
  });

  it('does not narrow when the folders query failed (a 5xx must not blank the list)', async () => {
    server.use(http.get(`${BASE}/elitea_core/entity_folders/prompt_lib/${PROJECT}/skill`, () => HttpResponse.json({ error: 'x' }, { status: 500 })));
    renderInRouter(<Probe projectId={PROJECT} />, '/skills/all?entity_folder=1');
    await new Promise((r) => setTimeout(r, 60));
    expect(screen.getByTestId('probe')).toHaveTextContent('false|undefined|true|true');
  });

  it('does not narrow when the query is off (no project) and leaves the param alone', async () => {
    const { router } = renderInRouter(<Probe projectId={undefined} />, '/skills/all?entity_folder=1');
    await new Promise((r) => setTimeout(r, 60));
    expect(screen.getByTestId('probe')).toHaveTextContent('false|undefined|true|true');
    expect(router.state.location.search).toMatchObject({ entity_folder: '1' });
  });

  it('ignores and clears a stale id that is not in this list', async () => {
    serveFolders('skill', [folder]);
    const { router } = renderInRouter(<Probe projectId={PROJECT} />, '/skills/all?entity_folder=99');
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty('entity_folder', '99'));
    expect(screen.getByTestId('probe')).toHaveTextContent('false|undefined|true|true');
  });
});
