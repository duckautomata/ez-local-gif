import { describe, expect, it } from 'vitest';
import { stageLabel } from './stages';

describe('stageLabel', () => {
  it('labels every jobs stage, the Phase 5b matte pass included, and falls back to the id', () => {
    expect(stageLabel('probe')).toBe('Probing source');
    expect(stageLabel('matte')).toBe('AI matte');
    expect(stageLabel('master')).toBe('Decoding frames');
    expect(stageLabel('encode')).toBe('Encoding');
    expect(stageLabel('fit')).toBe('Fitting to size');
    expect(stageLabel('lint')).toBe('Discord lint');
    expect(stageLabel('verify')).toBe('Verifying');
    expect(stageLabel('done')).toBe('Done');
    expect(stageLabel('somethingnew')).toBe('somethingnew');
    expect(stageLabel('')).toBe('');
    expect(stageLabel(undefined)).toBe('');
    expect(stageLabel(null)).toBe('');
  });
});
