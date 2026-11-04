import test from 'node:test';
import assert from 'node:assert/strict';
import { topWords } from '../src/wordfreq.mjs';

test('counts words case-insensitively, most frequent first', () => {
  assert.deepEqual(topWords('the cat The dog THE cat'), [
    ['the', 3],
    ['cat', 2],
    ['dog', 1],
  ]);
});

test('ties keep first-appearance order (not alphabetical)', () => {
  assert.deepEqual(topWords('pear apple fig apple pear fig kiwi'), [
    ['pear', 2],
    ['apple', 2],
    ['fig', 2],
    ['kiwi', 1],
  ]);
});

test('n limits the result', () => {
  assert.deepEqual(topWords('a b b c c c', 2), [
    ['c', 3],
    ['b', 2],
  ]);
});

test('non-ASCII letters are part of words', () => {
  assert.deepEqual(topWords('Straße straße, naïve NAÏVE! 東京 東京 東京'), [
    ['東京', 3],
    ['straße', 2],
    ['naïve', 2],
  ]);
  assert.deepEqual(topWords('Привет, мир! привет'), [
    ['привет', 2],
    ['мир', 1],
  ]);
});

test('composed and decomposed forms are the same word (NFC)', () => {
  assert.deepEqual(topWords('École école école'), [['école', 3]]);
});

test('inner apostrophes join, edge apostrophes do not; ’ is normalised', () => {
  assert.deepEqual(topWords("Don’t don't DON'T 'quoted' rock'n'roll"), [
    ["don't", 3],
    ['quoted', 1],
    ["rock'n'roll", 1],
  ]);
});

test('single inner hyphens join, edge and double hyphens split', () => {
  assert.deepEqual(topWords('e-mail E-Mail -dash- well--known'), [
    ['e-mail', 2],
    ['dash', 1],
    ['well', 1],
    ['known', 1],
  ]);
});

test('digits count as word characters', () => {
  assert.deepEqual(topWords('v2 2026 2026'), [
    ['2026', 2],
    ['v2', 1],
  ]);
});

test('stop words are normalised before comparison', () => {
  assert.deepEqual(topWords("the The don't x", undefined, { stopWords: ['THE', 'Don’t'] }), [['x', 1]]);
});

test('words that collide with Object.prototype keys are counted normally', () => {
  assert.deepEqual(topWords('constructor toString constructor hasOwnProperty'), [
    ['constructor', 2],
    ['tostring', 1],
    ['hasownproperty', 1],
  ]);
});

test('text without words yields an empty list', () => {
  assert.deepEqual(topWords(" -- ... '' ! "), []);
});
