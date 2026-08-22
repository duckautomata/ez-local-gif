import { afterEach, describe, expect, it } from 'vitest';
import type { Font } from './api';
import { DEFAULT_FONT, fontFamilies, loadFontFamilies, resetFontCache } from './fonts';

const faces: Font[] = [
  { family: 'Noto Sans', style: 'Regular', file: '/usr/share/fonts/noto/NotoSans-Regular.ttf' },
  { family: 'DejaVu Sans', style: 'Bold', file: '/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf' },
  { family: 'DejaVu Sans', style: 'Book', file: '/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf' },
  { family: 'Noto Sans', style: 'Bold', file: '/usr/share/fonts/noto/NotoSans-Bold.ttf' },
  { family: 'Comic Neue', style: 'Regular', file: '/fonts/ComicNeue.ttf' },
  { family: 'Bad, Name', style: 'Regular', file: '/fonts/bad.ttf' }, // the graph refuses a comma
  { family: '  ', style: 'Regular', file: '/fonts/blank.ttf' },
];

describe('fontFamilies', () => {
  it('reduces faces to unique drawtext-safe families, default first, rest sorted', () => {
    expect(fontFamilies(faces)).toEqual(['DejaVu Sans', 'Comic Neue', 'Noto Sans']);
  });
  it('falls back to the default when the endpoint is empty', () => {
    expect(fontFamilies([])).toEqual([DEFAULT_FONT]);
    expect(fontFamilies(null)).toEqual([DEFAULT_FONT]);
    expect(fontFamilies(undefined)).toEqual([DEFAULT_FONT]);
    expect(DEFAULT_FONT).toBe('DejaVu Sans'); // recipe.TextParams' zero value
  });
});

describe('loadFontFamilies', () => {
  afterEach(() => resetFontCache());

  it('caches a successful list', async () => {
    let n = 0;
    const fetcher = async () => {
      n++;
      return faces;
    };
    expect(await loadFontFamilies(fetcher)).toEqual(['DejaVu Sans', 'Comic Neue', 'Noto Sans']);
    expect(await loadFontFamilies(fetcher)).toHaveLength(3);
    expect(n).toBe(1);
  });

  it('never rejects: an unreachable endpoint yields the default and is asked again next time', async () => {
    let n = 0;
    const failing = async () => {
      n++;
      throw new Error('HTTP 404');
    };
    expect(await loadFontFamilies(failing)).toEqual([DEFAULT_FONT]);
    expect(await loadFontFamilies(failing)).toEqual([DEFAULT_FONT]);
    expect(n).toBe(2);
    expect(await loadFontFamilies(async () => faces)).toHaveLength(3);
  });
});
