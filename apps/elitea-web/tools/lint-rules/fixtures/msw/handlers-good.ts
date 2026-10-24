import { http, HttpResponse } from 'msw';

interface User {
  id: number;
  name: string;
}

// R-M2 GREEN: the body is a typed module constant.
const USERS: User[] = [{ id: 1, name: 'Ada' }];

export const handlers = [
  http.get('/api/v2/users', () => HttpResponse.json(USERS)),
];
