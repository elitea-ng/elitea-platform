import { describe, expect, it } from 'vitest';

import { authorDisplayName, isCurrentUserAuthor, isDeletedAuthor, isSameAuthor } from './selectors';
import type { Author } from './types';

const author = (id: string, name: string, email = `${id}@example.com`): Author => ({ id, name, email });

describe('authorDisplayName', () => {
  it('returns the name when non-blank', () => {
    expect(authorDisplayName(author('1', 'Ada Lovelace'))).toBe('Ada Lovelace');
  });

  it('falls back to email when the name is blank', () => {
    expect(authorDisplayName(author('1', '   ', 'ada@example.com'))).toBe('ada@example.com');
  });

  it('says "Deleted user" for an author whose account is gone, and never throws (#6702)', () => {
    // The server keeps the id and empties both name fields for a deleted account.
    expect(authorDisplayName(author('7', '', ''))).toBe('Deleted user');
    expect(authorDisplayName({ id: '7', name: null, email: null })).toBe('Deleted user');
    expect(authorDisplayName({ id: '7' })).toBe('Deleted user');
    expect(authorDisplayName(undefined)).toBe('Deleted user');
  });
});

describe('isDeletedAuthor (#6702)', () => {
  it('is true only for an author object with no name and no email', () => {
    expect(isDeletedAuthor(author('7', '', ''))).toBe(true);
    expect(isDeletedAuthor({ id: '7' })).toBe(true);
    expect(isDeletedAuthor(author('7', 'Ada'))).toBe(false);
    expect(isDeletedAuthor(author('7', '', 'ada@example.com'))).toBe(false);
    expect(isDeletedAuthor(undefined)).toBe(false);
    expect(isDeletedAuthor(null)).toBe(false);
  });
});

describe('isSameAuthor', () => {
  it('is true for matching ids regardless of other fields', () => {
    expect(isSameAuthor(author('1', 'A'), author('1', 'B', 'b@example.com'))).toBe(true);
  });

  it('is false for different ids', () => {
    expect(isSameAuthor(author('1', 'A'), author('2', 'A'))).toBe(false);
  });
});

describe('isCurrentUserAuthor', () => {
  it('is true when the id matches', () => {
    expect(isCurrentUserAuthor(author('1', 'A'), '1')).toBe(true);
  });

  it('is false when the id differs', () => {
    expect(isCurrentUserAuthor(author('1', 'A'), '2')).toBe(false);
  });

  it('is false when there is no current user', () => {
    expect(isCurrentUserAuthor(author('1', 'A'), undefined)).toBe(false);
  });
});
