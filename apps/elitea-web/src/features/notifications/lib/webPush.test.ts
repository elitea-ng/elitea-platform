import { describe, expect, it, vi } from 'vitest';

import { createFakePushBrowser } from './webPush.fake';
import { base64UrlToBytes, disableWebPush, enableWebPush, readWebPushState, webPushPlace } from './webPush';

const place = webPushPlace('/app/', '/api/v2', '7');
// 65 bytes: an uncompressed P-256 point starts with 0x04.
const KEY = btoa(String.fromCharCode(4, ...Array.from({ length: 64 }, (_, i) => i)))
  .replace(/\+/g, '-')
  .replace(/\//g, '_')
  .replace(/=+$/, '');

describe('webPushPlace', () => {
  it('puts the worker at the base path, scoped to it, with the REST base and project in its query', () => {
    expect(place).toEqual({ scope: '/app/', scriptUrl: '/app/push-sw.js?api=%2Fapi%2Fv2&project=7' });
    expect(webPushPlace('', '/api/v2', '7').scope).toBe('/');
    expect(webPushPlace('/app', '/api/v2', '7').scope).toBe('/app/');
  });
});

describe('base64UrlToBytes', () => {
  it('decodes an unpadded base64url key', () => {
    const bytes = base64UrlToBytes(KEY);
    expect(bytes).toHaveLength(65);
    expect(bytes[0]).toBe(4);
    expect(bytes[64]).toBe(63);
    expect(Array.from(base64UrlToBytes('-_8'))).toEqual([251, 255]);
  });
});

describe('readWebPushState', () => {
  it('is unsupported without a service worker or PushManager', async () => {
    expect(await readWebPushState(createFakePushBrowser({ supported: false }).env, place.scope)).toBe('unsupported');
  });

  it('is blocked when the browser refused notifications', async () => {
    const browser = createFakePushBrowser();
    browser.permission = 'denied';
    expect(await readWebPushState(browser.env, place.scope)).toBe('blocked');
  });

  it('is off before the person was asked, and with permission but no subscription', async () => {
    const browser = createFakePushBrowser();
    expect(await readWebPushState(browser.env, place.scope)).toBe('off');
    browser.permission = 'granted';
    expect(await readWebPushState(browser.env, place.scope)).toBe('off');
  });

  it('is on with a subscription in this scope', async () => {
    const browser = createFakePushBrowser();
    browser.permission = 'granted';
    browser.registered = { url: place.scriptUrl, scope: place.scope };
    browser.subscription = { endpoint: 'https://push.example/x', key: null };
    expect(await readWebPushState(browser.env, place.scope)).toBe('on');
  });
});

describe('enableWebPush', () => {
  it('asks permission, registers the worker, subscribes with the VAPID key and posts the subscription', async () => {
    const browser = createFakePushBrowser();
    const save = vi.fn().mockResolvedValue({ id: 's-1' });
    expect(await enableWebPush(browser.env, place, KEY, save)).toBe('on');
    expect(browser.calls).toEqual(['requestPermission', 'register', 'subscribe']);
    expect(browser.registered).toEqual({ url: place.scriptUrl, scope: '/app/' });
    expect(Array.from(browser.subscription?.key ?? [])).toEqual(Array.from(base64UrlToBytes(KEY)));
    expect(save).toHaveBeenCalledWith({ endpoint: 'https://push.example/ep-1', keys: { p256dh: 'p-key', auth: 'a-key' } });
  });

  it('does not ask again when permission was given, and reuses a subscription that exists', async () => {
    const browser = createFakePushBrowser();
    browser.permission = 'granted';
    browser.subscription = { endpoint: 'https://push.example/old', key: null };
    const save = vi.fn().mockResolvedValue({ id: 's-1' });
    expect(await enableWebPush(browser.env, place, KEY, save)).toBe('on');
    expect(browser.calls).toEqual(['register']);
    expect(save).toHaveBeenCalledWith(expect.objectContaining({ endpoint: 'https://push.example/old' }));
  });

  it('ends blocked when the person refuses, and off when they only close the prompt', async () => {
    const refused = createFakePushBrowser();
    refused.answer = 'denied';
    const save = vi.fn();
    expect(await enableWebPush(refused.env, place, KEY, save)).toBe('blocked');
    const closed = createFakePushBrowser();
    closed.answer = 'default';
    expect(await enableWebPush(closed.env, place, KEY, save)).toBe('off');
    expect(save).not.toHaveBeenCalled();
    expect(refused.registered).toBeNull();
  });

  it('undoes the browser subscription when Main refuses it', async () => {
    const browser = createFakePushBrowser();
    const save = vi.fn().mockRejectedValue(new Error('422'));
    await expect(enableWebPush(browser.env, place, KEY, save)).rejects.toThrow('422');
    expect(browser.subscription).toBeNull();
    expect(browser.calls).toContain('unsubscribe');
  });

  it('does nothing in a browser without push', async () => {
    const save = vi.fn();
    expect(await enableWebPush(createFakePushBrowser({ supported: false }).env, place, KEY, save)).toBe('unsupported');
    expect(save).not.toHaveBeenCalled();
  });
});

describe('disableWebPush', () => {
  it('deletes the endpoint at Main, unsubscribes and removes the worker', async () => {
    const browser = createFakePushBrowser();
    browser.permission = 'granted';
    browser.registered = { url: place.scriptUrl, scope: place.scope };
    browser.subscription = { endpoint: 'https://push.example/x', key: null };
    const remove = vi.fn().mockResolvedValue(undefined);
    expect(await disableWebPush(browser.env, place.scope, remove)).toBe('off');
    expect(remove).toHaveBeenCalledWith('https://push.example/x');
    expect(browser.calls).toEqual(['unsubscribe', 'unregister']);
    expect(await readWebPushState(browser.env, place.scope)).toBe('off');
  });

  it('undoes the browser side even when Main cannot be reached', async () => {
    const browser = createFakePushBrowser();
    browser.permission = 'granted';
    browser.registered = { url: place.scriptUrl, scope: place.scope };
    browser.subscription = { endpoint: 'https://push.example/x', key: null };
    expect(await disableWebPush(browser.env, place.scope, vi.fn().mockRejectedValue(new Error('offline')))).toBe('off');
    expect(browser.subscription).toBeNull();
    expect(browser.registered).toBeNull();
  });
});
