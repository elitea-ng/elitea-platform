import test from 'node:test';
import assert from 'node:assert/strict';
import { parse, compare, sort } from '../src/semver.mjs';

test('parse splits all parts', () => {
  assert.deepEqual(parse('1.2.3-alpha.1+build.5'), {
    major: 1,
    minor: 2,
    patch: 3,
    prerelease: ['alpha', '1'],
    build: ['build', '5'],
  });
});

test('parse rejects leading zeros in major/minor/patch', () => {
  assert.throws(() => parse('01.2.3'), Error);
  assert.throws(() => parse('1.02.3'), Error);
  assert.throws(() => parse('1.2.03'), Error);
});

test('parse rejects leading zeros in numeric prerelease identifiers only', () => {
  assert.throws(() => parse('1.0.0-01'), Error);
  assert.throws(() => parse('1.0.0-rc.007'), Error);
  assert.doesNotThrow(() => parse('1.0.0-0a'), 'alphanumeric identifiers may start with 0');
  assert.doesNotThrow(() => parse('1.0.0+001'), 'build identifiers may have leading zeros');
});

test('parse rejects empty identifiers', () => {
  assert.throws(() => parse('1.0.0-alpha..1'), Error);
  assert.throws(() => parse('1.0.0-'), Error);
  assert.throws(() => parse('1.0.0+'), Error);
  assert.throws(() => parse('1.0.0-a.'), Error);
});

test('release outranks its prereleases', () => {
  assert.equal(compare('1.0.0-rc.1', '1.0.0'), -1);
  assert.equal(compare('1.0.0', '1.0.0-rc.1'), 1);
  assert.equal(compare('1.0.0-rc.1', '0.9.9'), 1);
});

test('numeric identifiers compare numerically', () => {
  assert.equal(compare('1.0.0-beta.2', '1.0.0-beta.11'), -1);
  assert.equal(compare('1.0.0-10', '1.0.0-9'), 1);
});

test('numeric identifiers rank below alphanumeric ones', () => {
  assert.equal(compare('1.0.0-1', '1.0.0-alpha'), -1);
  assert.equal(compare('1.0.0-alpha.1', '1.0.0-alpha.beta'), -1);
});

test('more identifiers wins when the shared prefix is equal', () => {
  assert.equal(compare('1.0.0-alpha', '1.0.0-alpha.1'), -1);
  assert.equal(compare('1.0.0-alpha.1.0', '1.0.0-alpha.1'), 1);
});

test('build metadata is ignored for precedence', () => {
  assert.equal(compare('1.0.0+a', '1.0.0+b'), 0);
  assert.equal(compare('1.0.0-rc.1+x', '1.0.0-rc.1'), 0);
});

test('sort orders the semver.org example chain and does not mutate input', () => {
  const expected = [
    '1.0.0-alpha',
    '1.0.0-alpha.1',
    '1.0.0-alpha.beta',
    '1.0.0-beta',
    '1.0.0-beta.2',
    '1.0.0-beta.11',
    '1.0.0-rc.1',
    '1.0.0',
    '2.0.0',
    '2.1.0',
    '2.1.1',
  ];
  const input = [...expected].reverse();
  const snapshot = [...input];
  assert.deepEqual(sort(input), expected);
  assert.deepEqual(input, snapshot, 'input array was mutated');
});
