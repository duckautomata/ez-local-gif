import { describe, expect, it } from 'vitest';
import { isAnimatedFormat, isHash, isStaticFormat, OUTPUT_FORMATS, sourceHashFromSearch, sourceURL } from './api';

const HASH = '0123456789abcdef'.repeat(4);

describe('source URL round trip', () => {
  it('reads ?src= only when it is a sha256 hex hash', () => {
    expect(sourceHashFromSearch(`?src=${HASH}`)).toBe(HASH);
    expect(sourceHashFromSearch(`?x=1&src=${HASH}&y=2`)).toBe(HASH);
    expect(sourceHashFromSearch('?src=../etc')).toBeNull();
    expect(sourceHashFromSearch(`?src=${HASH.toUpperCase()}`)).toBeNull();
    expect(sourceHashFromSearch('?src=')).toBeNull();
    expect(sourceHashFromSearch('')).toBeNull();
  });
  it('builds the address "edit as source" opens', () => {
    expect(sourceURL(HASH)).toBe(`/?src=${HASH}`);
    expect(sourceURL(null)).toBe('/');
    expect(sourceHashFromSearch(sourceURL(HASH).slice(1))).toBe(HASH);
  });
  it('isHash mirrors recipe.IsHash', () => {
    expect(isHash(HASH)).toBe(true);
    expect(isHash(HASH.slice(1))).toBe(false);
    expect(isHash(HASH.replace('a', 'g'))).toBe(false);
  });
});

describe('format classes', () => {
  it('mirror recipe.IsAnimatedFormat / IsStaticFormat', () => {
    expect(OUTPUT_FORMATS.filter(isAnimatedFormat)).toEqual(['gif', 'webp', 'apng', 'avif']);
    expect(OUTPUT_FORMATS.filter(isStaticFormat)).toEqual(['png', 'jpeg']);
    expect(isAnimatedFormat('frames')).toBe(false);
    expect(isStaticFormat('frames')).toBe(false);
  });
});
