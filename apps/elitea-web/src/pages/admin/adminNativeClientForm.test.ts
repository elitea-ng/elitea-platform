import { describe, expect, it } from 'vitest';

import {
  initialNativeClientForm,
  isEditableNativeClient,
  nativeClientDraft,
  placeNativeClientReasons,
  splitRedirectUris,
} from './adminNativeClientForm';

const CLIENT = {
  client_id: 'com.example.app',
  display_name: 'Example',
  redirect_uris: ['com.example.app:/cb', 'http://127.0.0.1/cb'],
  enabled: false,
  min_client_version: '2.0.0',
  source: 'db',
  overridden_file: true,
  active_devices: 0,
};

describe('splitRedirectUris', () => {
  it('keeps one trimmed URI per non-blank line', () => {
    expect(splitRedirectUris('  a:/x \n\n   \nb:/y\n')).toEqual(['a:/x', 'b:/y']);
  });
});

describe('initialNativeClientForm / nativeClientDraft', () => {
  it('round-trips a stored client, including a DISABLED one', () => {
    // `enabled` must come from the row: defaulting it to true would re-enable
    // a disabled client the moment an operator edited its name.
    const draft = nativeClientDraft(initialNativeClientForm(CLIENT));
    expect(draft).toEqual({
      clientId: 'com.example.app',
      displayName: 'Example',
      redirectUris: ['com.example.app:/cb', 'http://127.0.0.1/cb'],
      enabled: false,
      minClientVersion: '2.0.0',
    });
  });

  it('starts a new client enabled with nothing else filled', () => {
    expect(initialNativeClientForm(undefined)).toEqual({
      clientId: '',
      displayName: '',
      redirectUris: '',
      enabled: true,
      minClientVersion: '',
    });
  });
});

describe('placeNativeClientReasons', () => {
  it('places field reasons, prefixes per-URI reasons with the URI, and keeps unknown keys', () => {
    const placed = placeNativeClientReasons(
      {
        client_id: 'bad id',
        'redirect_uris[1]': 'loopback must not carry a port',
        redirect_uris: 'at least one redirect URI is required',
        surprise: 'something new',
      },
      ['com.example.app:/cb', 'http://127.0.0.1:8080/cb'],
    );
    expect(placed.clientId).toBe('bad id');
    expect(placed.redirectUris).toEqual([
      'at least one redirect URI is required',
      'http://127.0.0.1:8080/cb: loopback must not carry a port',
    ]);
    expect(placed.other).toEqual(['surprise: something new']);
  });

  it('keeps a per-URI reason whose index is past the list', () => {
    expect(placeNativeClientReasons({ 'redirect_uris[4]': 'bad' }, []).redirectUris).toEqual(['bad']);
  });
});

describe('isEditableNativeClient', () => {
  it('is false only for the file layer', () => {
    expect(isEditableNativeClient(CLIENT)).toBe(true);
    expect(isEditableNativeClient({ ...CLIENT, source: 'file' })).toBe(false);
  });
});
