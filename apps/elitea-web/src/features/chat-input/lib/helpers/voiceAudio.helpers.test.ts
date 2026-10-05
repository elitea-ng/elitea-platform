import { describe, expect, it } from 'vitest';

import {
  UtteranceSegmenter,
  encodeWav,
  float32ToPcm16Buffer,
  speechInstructionsFor,
  splitSpeechSegments,
} from './voiceAudio.helpers';

describe('splitSpeechSegments (indexer_tts.py `_split_sentences` parity)', () => {
  it('splits on sentence punctuation and records char_end before the whitespace', () => {
    const text = 'Hello there. How are you? Fine!';
    expect(splitSpeechSegments(text)).toEqual([
      { text: 'Hello there.', charEnd: 12 },
      { text: 'How are you?', charEnd: 25 },
      { text: 'Fine!', charEnd: text.length },
    ]);
  });

  it('treats newlines as boundaries and trims each sentence', () => {
    const text = '  First line\n\nSecond line  ';
    expect(splitSpeechSegments(text)).toEqual([
      { text: 'First line', charEnd: 12 },
      { text: 'Second line', charEnd: 25 },
    ]);
  });

  it('does not split a decimal point or an abbreviation without a following space', () => {
    expect(splitSpeechSegments('Pi is 3.14 today.')).toEqual([{ text: 'Pi is 3.14 today.', charEnd: 17 }]);
  });

  it('returns nothing for whitespace-only text', () => {
    expect(splitSpeechSegments('   \n  ')).toEqual([]);
  });

  it('cuts a sentence longer than the provider limit at a space, keeping every charEnd inside the text', () => {
    const text = 'aaaa bbbb cccc dddd';
    const segments = splitSpeechSegments(text, 10);
    expect(segments.map((s) => s.text)).toEqual(['aaaa bbbb', 'cccc dddd']);
    expect(segments.every((s) => s.text.length <= 10)).toBe(true);
    expect(segments[0]?.charEnd).toBe(9);
    expect(segments[1]?.charEnd).toBe(text.length);
  });

  it('cuts a sentence with no space hard at the limit', () => {
    expect(splitSpeechSegments('abcdefghij', 4).map((s) => s.text)).toEqual(['abcd', 'efgh', 'ij']);
  });
});

describe('speechInstructionsFor', () => {
  it('pins the persona for the gpt-4o TTS family only', () => {
    expect(speechInstructionsFor('gpt-4o-mini-tts')).toContain('calm and warm');
    expect(speechInstructionsFor('tts-1')).toBeUndefined();
    expect(speechInstructionsFor('gpt-4o')).toBeUndefined();
  });
});

describe('float32ToPcm16Buffer', () => {
  it('converts a Float32Array to a 16-bit PCM ArrayBuffer of the same length', () => {
    const input = new Float32Array([0, 0.5, -0.5, 1, -1]);
    const buffer = float32ToPcm16Buffer(input);
    expect(buffer.byteLength).toBe(input.length * 2);
    const view = new Int16Array(buffer);
    expect(view[0]).toBe(0);
    expect(view[3]).toBe(0x7fff);
    expect(view[4]).toBe(-0x8000);
  });

  it('clamps out-of-range samples to [-1, 1] before scaling', () => {
    const view = new Int16Array(float32ToPcm16Buffer(new Float32Array([2, -2])));
    expect(view[0]).toBe(0x7fff);
    expect(view[1]).toBe(-0x8000);
  });
});

describe('encodeWav', () => {
  it('writes a 16-bit mono PCM RIFF header the transcription route accepts', async () => {
    const blob = encodeWav(new Float32Array([0, 0.5, -0.5]), 24000);
    expect(blob.type).toBe('audio/wav');
    const bytes = new DataView(await blob.arrayBuffer());
    const ascii = (offset: number, length: number): string =>
      String.fromCharCode(...Array.from({ length }, (_, i) => bytes.getUint8(offset + i)));
    expect(ascii(0, 4)).toBe('RIFF');
    expect(ascii(8, 4)).toBe('WAVE');
    expect(bytes.getUint16(20, true)).toBe(1); // PCM
    expect(bytes.getUint16(22, true)).toBe(1); // mono
    expect(bytes.getUint32(24, true)).toBe(24000);
    expect(bytes.getUint16(34, true)).toBe(16);
    expect(ascii(36, 4)).toBe('data');
    expect(bytes.getUint32(40, true)).toBe(6);
    expect(bytes.byteLength).toBe(44 + 6);
  });
});

describe('UtteranceSegmenter (sio/asr.py whisper VAD parity)', () => {
  const loud = (n = 7200): Float32Array => new Float32Array(n).fill(0.2);
  const quiet = (n = 7200): Float32Array => new Float32Array(n).fill(0.001);

  it('ignores silence until speech starts', () => {
    const segmenter = new UtteranceSegmenter();
    expect(segmenter.push(quiet())).toEqual([]);
    expect(segmenter.flush()).toEqual([]);
  });

  it('reports speech-start once, then a segment after two silent frames, including the trailing silence', () => {
    const segmenter = new UtteranceSegmenter();
    expect(segmenter.push(loud())).toEqual([{ kind: 'speech-start' }]);
    expect(segmenter.push(loud())).toEqual([]);
    expect(segmenter.push(quiet())).toEqual([]);
    const [event] = segmenter.push(quiet());
    expect(event?.kind).toBe('segment');
    expect(event?.kind === 'segment' ? event.samples.length : 0).toBe(4 * 7200);
  });

  it('a speech frame in the silence window resets the silence count', () => {
    const segmenter = new UtteranceSegmenter();
    segmenter.push(loud());
    segmenter.push(quiet());
    segmenter.push(loud());
    expect(segmenter.push(quiet())).toEqual([]);
    expect(segmenter.push(quiet())[0]?.kind).toBe('segment');
  });

  it('discards an utterance shorter than the minimum', () => {
    const segmenter = new UtteranceSegmenter({ minSamples: 2400 });
    segmenter.push(loud(100));
    segmenter.push(quiet(100));
    expect(segmenter.push(quiet(100))).toEqual([{ kind: 'discard' }]);
  });

  it('cuts an utterance at the maximum length so one upload stays bounded', () => {
    const segmenter = new UtteranceSegmenter({ maxSamples: 14400 });
    segmenter.push(loud());
    const events = segmenter.push(loud());
    expect(events.map((e) => e.kind)).toEqual(['segment']);
    // The next loud frame starts a new utterance.
    expect(segmenter.push(loud())).toEqual([{ kind: 'speech-start' }]);
  });

  it('flush ends the utterance in progress', () => {
    const segmenter = new UtteranceSegmenter();
    segmenter.push(loud());
    expect(segmenter.flush()[0]?.kind).toBe('segment');
    expect(segmenter.flush()).toEqual([]);
  });

  it('ignores an empty frame', () => {
    expect(new UtteranceSegmenter().push(new Float32Array(0))).toEqual([]);
  });
});
