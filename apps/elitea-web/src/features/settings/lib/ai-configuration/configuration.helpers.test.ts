/**
 * configuration.helpers.test.ts — `isConfigurationEditable`.
 *
 * MAJOR regression (D3). The helper compared `configuration.project_id` with
 * the selected project id using `===`, having cast the row's id to
 * `string | undefined`. The cast was a lie: the configuration LIST route
 * answers `"project_id": 2`, a number
 * (`CurrentConfigurationDTO.ProjectID` is an `int32`), while the single-row
 * route used to answer the string `"2"`
 * (`Configuration.ProjectID` was a `string`). `2 === '2'` is false, so every
 * card on the AI-Configuration screen reported "No edit permissions" and no
 * configuration in the project could be opened.
 *
 * The Go side now agrees on the number (`configurations/dto_test.go` pins
 * it). This helper still normalises both ends, because a client must not
 * depend on which of two routes fed it the row.
 */
import { describe, expect, it } from 'vitest';

import { isConfigurationEditable } from './configuration.helpers';

describe('isConfigurationEditable', () => {
  it('matches a NUMERIC project_id against the string project id (the list route shape)', () => {
    expect(isConfigurationEditable({ project_id: 2 }, '2', true)).toBe(true);
  });

  it('matches a STRING project_id against the same id (the single-row route shape)', () => {
    expect(isConfigurationEditable({ project_id: '2' }, '2', true)).toBe(true);
  });

  it('still refuses a row owned by another project, in either shape', () => {
    expect(isConfigurationEditable({ project_id: 3 }, '2', true)).toBe(false);
    expect(isConfigurationEditable({ project_id: '3' }, '2', true)).toBe(false);
  });

  it('honours the permission flag once the project matches', () => {
    expect(isConfigurationEditable({ project_id: 2 }, '2', false)).toBe(false);
  });

  /**
   * An absent id must never read as "same project". `String(undefined)` is
   * `'undefined'` and `String(null)` is `'null'`, so a naive `String()` on
   * both sides would happily match two absent ids against each other.
   */
  it('refuses when either id is absent', () => {
    expect(isConfigurationEditable({}, '2', true)).toBe(false);
    expect(isConfigurationEditable({ project_id: null }, '2', true)).toBe(false);
    expect(isConfigurationEditable({ project_id: undefined }, '', true)).toBe(false);
    expect(isConfigurationEditable({ project_id: '' }, '', true)).toBe(false);
  });
});
