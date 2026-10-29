import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';

import { afterAll, beforeAll, expect, it } from 'vitest';

// R-M5 RED: no handler covers this URL. With onUnhandledRequest:'error'
// (src/test/setup.ts) the request is rejected and this test MUST fail.
//
// A real server answers 200 on the port, so WITHOUT the fence this test
// passes. Without that server the request would fail with ECONNREFUSED
// whether the fence works or not, and the RED half would prove nothing: a
// renamed or ignored msw option (msw 3 renames it to `onUnhandledFrame`) would
// still look fenced.
let upstream: Server;
let port = 0;

beforeAll(async () => {
  upstream = createServer((_req, res) => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end('{"ok":true}');
  });
  await new Promise<void>((done) => upstream.listen(0, '127.0.0.1', done));
  port = (upstream.address() as AddressInfo).port;
});

afterAll(async () => {
  await new Promise<void>((done) => upstream.close(() => done()));
});

it('unmocked request would pass without the R-M5 fence', async () => {
  const response = await fetch(`http://127.0.0.1:${port}/api/v2/unmocked`);
  expect(response.ok).toBe(true);
});
