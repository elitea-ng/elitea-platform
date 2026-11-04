/*
 * Elitea web push service worker (ADR-0045 decision 6, U3 contract §4).
 *
 * Served from the app's public directory at the app's base path
 * (`/app/push-sw.js`), so its scope is the whole app. The app registers it
 * only when the person turns on "Web push in this browser" (Settings ›
 * Notifications) and removes it when they turn that off
 * (`src/features/notifications/lib/webPush.ts`).
 *
 * The payload is content-free: a type, ids and an outcome. The notice text is
 * fixed here and nothing is fetched to fill it. The text is English: a worker
 * cannot read the app's catalogue.
 *
 * Plain script, no bundler. Everything hangs off the `self` it is given, so
 * `src/features/notifications/lib/pushServiceWorker.test.ts` runs this file
 * with a fake `self`.
 */
'use strict';

(function (sw) {
  var TEXT = {
    completed: { title: 'Run finished', body: 'A run is done.' },
    failed: { title: 'Run failed', body: 'A run stopped with an error.' },
    waiting: { title: 'Run needs you', body: 'A run waits for your answer.' },
    limit_reached: { title: 'Run stopped at its limit', body: 'A run reached its chain limit.' },
  };

  function readPayload(event) {
    try {
      return event.data ? event.data.json() : null;
    } catch {
      return null;
    }
  }

  function noticeText(payload) {
    if (!payload) return null;
    if (payload.type === 'agent_approval_required') return TEXT.waiting;
    if (payload.type === 'run_finished') return TEXT[payload.outcome] || TEXT.completed;
    return null;
  }

  function present(value) {
    return value !== undefined && value !== null && String(value) !== '';
  }

  // `/<project>/chat?conversation=<uuid>` selects the project, then opens the conversation (ROUTE-070).
  function targetUrl(scope, payload) {
    if (!present(payload.project_id) || !present(payload.conversation_uuid)) return scope;
    return scope + encodeURIComponent(String(payload.project_id)) + '/chat?conversation=' + encodeURIComponent(String(payload.conversation_uuid));
  }

  function showsConversation(clientUrl, conversationUuid) {
    if (!present(conversationUuid)) return false;
    try {
      var url = new URL(clientUrl);
      var uuid = String(conversationUuid);
      return url.pathname.slice(-(uuid.length + 6)) === '/chat/' + uuid || url.searchParams.get('conversation') === uuid;
    } catch {
      return false;
    }
  }

  function windowClients() {
    return sw.clients.matchAll({ type: 'window', includeUncontrolled: true });
  }

  function onPush(payload) {
    var text = noticeText(payload);
    if (!text) return Promise.resolve();
    return windowClients().then(function (clients) {
      var watching = clients.some(function (client) {
        return client.focused && showsConversation(client.url, payload.conversation_uuid);
      });
      if (watching) return undefined;
      var tag = 'elitea-' + (present(payload.notification_id) ? payload.notification_id : payload.type);
      return sw.registration.showNotification(text.title, {
        body: text.body,
        tag: tag,
        data: { url: targetUrl(sw.registration.scope, payload) },
      });
    });
  }

  function onClick(notification) {
    notification.close();
    var scope = sw.registration.scope;
    var url = (notification.data && notification.data.url) || scope;
    return windowClients().then(function (clients) {
      var app = clients.find(function (client) {
        return client.url.indexOf(scope) === 0;
      });
      if (!app) return sw.clients.openWindow(url);
      return app
        .focus()
        .then(function (focused) {
          return (focused || app).navigate(url);
        })
        .catch(function () {
          return sw.clients.openWindow(url);
        });
    });
  }

  function subscriptionBody(subscription) {
    var json = subscription.toJSON();
    var keys = json.keys || {};
    return { endpoint: subscription.endpoint, keys: { p256dh: keys.p256dh || '', auth: keys.auth || '' } };
  }

  // The worker URL carries the REST base and a project the person can read (`webPushPlace`).
  function subscriptionsUrl() {
    var params = new URL(sw.location.href).searchParams;
    var api = (params.get('api') || '/api/v2').replace(/\/+$/, '');
    return api + '/notifications/push_subscriptions/prompt_lib/' + encodeURIComponent(params.get('project') || '');
  }

  function send(method, body) {
    return sw.fetch(subscriptionsUrl(), {
      method: method,
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  }

  // The push service replaced or expired the subscription: subscribe again and tell Main the new endpoint.
  function onSubscriptionChange(event) {
    var old = event.oldSubscription || null;
    var next = event.newSubscription
      ? Promise.resolve(event.newSubscription)
      : old
        ? sw.registration.pushManager.subscribe(old.options)
        : Promise.resolve(null);
    return next.then(function (subscription) {
      if (!subscription) return undefined;
      return send('POST', subscriptionBody(subscription)).then(function () {
        if (old && old.endpoint !== subscription.endpoint) {
          return send('DELETE', { endpoint: old.endpoint }).catch(function () {
            return undefined;
          });
        }
        return undefined;
      });
    });
  }

  sw.addEventListener('install', function () {
    sw.skipWaiting();
  });
  sw.addEventListener('activate', function (event) {
    event.waitUntil(sw.clients.claim());
  });
  sw.addEventListener('push', function (event) {
    var payload = readPayload(event);
    if (payload) event.waitUntil(onPush(payload));
  });
  sw.addEventListener('notificationclick', function (event) {
    event.waitUntil(onClick(event.notification));
  });
  sw.addEventListener('pushsubscriptionchange', function (event) {
    event.waitUntil(onSubscriptionChange(event));
  });
})(self);
