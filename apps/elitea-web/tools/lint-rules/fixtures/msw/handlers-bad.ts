import { http, HttpResponse } from 'msw';

// R-M2 RED: an inline literal body, and an untyped constant.
const USER = { id: 2, name: 'untyped constant' };

export const handlers = [
  http.get('/api/v2/users', () => HttpResponse.json({ id: 1, name: 'inline literal' })),
  http.get('/api/v2/users/2', () => HttpResponse.json(USER)),
];
