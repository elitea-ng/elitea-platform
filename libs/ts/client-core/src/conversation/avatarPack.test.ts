import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { AVATAR_PACK_SCHEMA, AVATAR_SLOTS, avatarPackOf, avatarSource, avatarStillUrl, isAgentMediaUrl } from './avatarPack';

const media = (ext: string, project = '1') => `/api/v2/agent_media/${project}/${'ab'.repeat(32)}.${ext}`;
const poster = media('png');

const pack = {
  schema: AVATAR_PACK_SCHEMA,
  slots: {
    still: { url: media('png'), media_type: 'image/png' },
    thinking: { url: media('webp') },
    speaking: { url: media('webm'), poster_url: poster },
  },
};

describe('AVATAR_SLOTS', () => {
  it('names the still and the eight agent states', () => {
    assert.deepEqual(AVATAR_SLOTS, ['still', 'idle', 'thinking', 'speaking', 'working', 'waiting', 'success', 'error', 'waking']);
  });
});

describe('avatarPackOf', () => {
  it('keeps a well-formed pack', () => {
    const got = avatarPackOf(pack);
    assert.equal(got?.slots.still?.url, media('png'));
    assert.equal(got?.slots.speaking?.poster_url, poster);
  });

  it('refuses null, an unknown schema and a pack without a still', () => {
    assert.equal(avatarPackOf(null), undefined);
    assert.equal(avatarPackOf({ ...pack, schema: 'v0' }), undefined);
    assert.equal(avatarPackOf({ schema: AVATAR_PACK_SCHEMA, slots: { idle: { url: media('webp') } } }), undefined);
    assert.equal(avatarPackOf({ schema: AVATAR_PACK_SCHEMA, slots: { still: { url: media('mp4'), poster_url: poster } } }), undefined);
  });

  it('drops a foreign url, an unknown slot and a video without a poster', () => {
    const got = avatarPackOf({
      schema: AVATAR_PACK_SCHEMA,
      slots: {
        still: { url: media('png') },
        idle: { url: 'https://evil.example/a.png' },
        working: { url: media('mp4') },
        dancing: { url: media('webp') },
      },
    });
    assert.deepEqual(Object.keys(got?.slots ?? {}), ['still']);
  });

  it('matches only content-addressed agent media', () => {
    assert.equal(isAgentMediaUrl(media('svg')), true);
    assert.equal(isAgentMediaUrl('/icons/1/a.png'), false);
    assert.equal(isAgentMediaUrl(`${media('png')}?x=1`), false);
  });
});

describe('avatarSource', () => {
  it('plays a state slot as its own motion', () => {
    assert.deepEqual(avatarSource(pack, undefined, 'thinking'), { kind: 'image', src: media('webp'), ownMotion: true });
    assert.deepEqual(avatarSource(pack, undefined, 'speaking'), { kind: 'video', src: media('webm'), poster, ownMotion: true });
  });

  it('shows the still with the default motion for a state with no slot', () => {
    assert.deepEqual(avatarSource(pack, '/icons/1/old.png', 'working'), { kind: 'image', src: media('png'), ownMotion: false });
  });

  it('shows the still alone when the motion is still, so a video never plays', () => {
    assert.deepEqual(avatarSource(pack, undefined, 'still'), { kind: 'image', src: media('png'), ownMotion: false });
  });

  it('falls back to meta.icon_meta without a pack, and to nothing without either', () => {
    assert.deepEqual(avatarSource(null, '/icons/1/old.png', 'thinking'), { kind: 'image', src: '/icons/1/old.png', ownMotion: false });
    assert.equal(avatarSource(undefined, undefined, 'thinking'), undefined);
    assert.equal(avatarStillUrl(null, '/icons/1/old.png'), '/icons/1/old.png');
    assert.equal(avatarStillUrl(pack, '/icons/1/old.png'), media('png'));
  });
});
