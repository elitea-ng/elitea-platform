import { describe, expect, it } from 'vitest';

import {
  buildApplicationAttachment,
  buildToolkitAttachment,
  getAttachmentPagePath,
  getToolkitAvailableTools,
  withApplicationVersion,
} from './attachmentTool.helpers';

describe('attachmentTool.helpers', () => {
  it('builds toolkit and MCP attachments preserving selected_tools as available_tools', () => {
    const attached = buildToolkitAttachment(
      {
        id: 7,
        type: 'github',
        name: 'GitHub',
        description: 'Repo access',
        settings: { selected_tools: ['list_issues', 'create_pr'] },
        author: { id: 9, name: 'Alice' },
      },
      { mcp: false, toolkitName: 'gh_main' },
    );

    expect(attached).toMatchObject({
      id: 7,
      type: 'github',
      name: 'GitHub',
      toolkit_name: 'gh_main',
      meta: { mcp: false },
      author_id: 9,
      settings: {
        selected_tools: ['list_issues', 'create_pr'],
        available_tools: ['list_issues', 'create_pr'],
      },
    });
    expect(getToolkitAvailableTools(attached)).toEqual(['list_issues', 'create_pr']);
  });

  it('builds application attachment and updates version cleanly', () => {
    const app = buildApplicationAttachment({
      id: 42,
      name: 'CodeReviewer',
      description: 'Reviews PRs',
      version_details: {
        id: 5,
        agent_type: 'classic',
        icon_meta: { icon_id: 'badge-a' },
        variables: [{ name: 'env', value: 'prod' }],
      },
    });

    expect(app).toMatchObject({
      type: 'application',
      name: 'CodeReviewer',
      agent_type: 'classic',
      settings: { application_id: 42, application_version_id: 5 },
      variables: [{ name: 'env', value: 'prod' }],
    });

    const updated = withApplicationVersion(app, { id: 6, icon_meta: { icon_id: 'badge-b' } });
    expect(updated.settings?.application_version_id).toBe(6);
    expect(updated.icon_meta).toEqual({ icon_id: 'badge-b' });
  });

  it('builds safe attachment page paths and rejects path-traversal/CRLF/encoded-slash ids', () => {
    expect(
      getAttachmentPagePath({
        type: 'application',
        agent_type: 'pipeline',
        settings: { application_id: 12, application_version_id: 34 },
      }),
    ).toBe('/pipelines/all/12/34');
    expect(
      getAttachmentPagePath({
        type: 'application',
        agent_type: 'classic',
        settings: { application_id: 12 },
      }),
    ).toBe('/agents/all/12');
    expect(getAttachmentPagePath({ id: 5, type: 'github' })).toBe('/toolkits/all/5');
    expect(getAttachmentPagePath({ id: 6, type: 'mcp', meta: { mcp: true } })).toBe('/mcps/all/6');

    // Security encoding checks: reject traversal, slashes, %2F, and CRLF
    expect(getAttachmentPagePath({ id: '../admin', type: 'github' })).toBeNull();
    expect(getAttachmentPagePath({ id: '5/../../etc', type: 'github' })).toBeNull();
    expect(getAttachmentPagePath({ id: '5%2Fadmin', type: 'github' })).toBeNull();
    expect(getAttachmentPagePath({ id: '5\r\nX-Injected: 1', type: 'github' })).toBeNull();
  });
});
