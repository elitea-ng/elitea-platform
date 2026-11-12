import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { DEFAULT_AVATARS, DEFAULT_AVATAR_KEYS, DEFAULT_AVATAR_NAMES, defaultAvatarKey, defaultAvatarOf } from './defaultAvatars';

describe('default avatars', () => {
  it('draws one image per name and keys them default:<name>', () => {
    assert.deepEqual(
      DEFAULT_AVATAR_KEYS,
      DEFAULT_AVATAR_NAMES.map((name) => `default:${name}`),
    );
    for (const name of DEFAULT_AVATAR_NAMES) {
      const avatar = defaultAvatarOf(defaultAvatarKey(name));
      assert.equal(avatar, DEFAULT_AVATARS[name]);
      assert.ok(avatar.shapes.length >= 2);
      assert.ok(avatar.shapes.some((shape) => shape.role === 'body'));
    }
  });

  it('answers undefined for an unknown key so the client draws the brand mark', () => {
    for (const key of [undefined, null, '', 'default:', 'default:dragon', 'owl', 'agent:7', 'DEFAULT:owl', 'default:owl ']) {
      assert.equal(defaultAvatarOf(key), undefined, String(key));
    }
  });

  it('holds no colour literal: shapes carry roles only', () => {
    for (const avatar of Object.values(DEFAULT_AVATARS)) {
      for (const shape of avatar.shapes) {
        assert.match(shape.role, /^(body|detail|accent)$/);
        assert.doesNotMatch(shape.d, /#|rgb|hsl/i);
      }
    }
  });
});
