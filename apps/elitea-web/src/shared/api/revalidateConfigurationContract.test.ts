/**
 * revalidateConfigurationContract.test.ts — `project_id` is a NUMBER on the
 * revalidate response.
 *
 * The Go handler answers `POST /configurations/revalidate/{project_id}/{config_id}`
 * with the detail route's `Configuration` projection, whose `ProjectID` is a Go
 * `int` (services/elitea-main/internal/api/v2/configurations/handler.go). It
 * therefore puts a JSON number on the wire.
 *
 * v2.yaml declared `project_id: { type: string }` for that response, so orval
 * generated `zod.string()` here. A zod parse of a CORRECT answer threw. The
 * spec now declares an integer, and this test pins the generated schema to the
 * shape the server sends, so a regeneration that reverts it fails here rather
 * than in a browser.
 */
import { describe, expect, it } from 'vitest';

import { RevalidateConfiguration200 } from '@/shared/api/generated/model/revalidateConfiguration200.zod';

const row = {
  id: 9,
  uuid: '00000000-0000-4000-8000-000000000009',
  name: 'vllm_creds',
  type: 'vllm',
  section: 'ai_credentials',
  status_ok: true,
};

describe('RevalidateConfiguration200', () => {
  it('accepts the integer project_id the server sends', () => {
    const parsed = RevalidateConfiguration200.parse({ ...row, project_id: 2 });

    expect(parsed.project_id).toBe(2);
  });

  it('refuses the string the spec used to declare', () => {
    expect(() => RevalidateConfiguration200.parse({ ...row, project_id: '2' })).toThrow();
  });

  it('keeps project_id optional, because the projection omits nothing else here', () => {
    expect(RevalidateConfiguration200.parse(row).project_id).toBeUndefined();
  });
});
