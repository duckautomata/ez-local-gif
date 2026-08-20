import { describe, expect, it, vi } from 'vitest';
import type { Source } from './api';
import { editAsSource, type OpenedTab } from './editsource';

const HASH = 'f'.repeat(64);
const src: Source = {
  hash: HASH,
  name: 'out.gif',
  size: 10,
  info: { format: 'gif', codec: 'gif', pixFmt: 'bgra', bits: 8, width: 1, height: 1, fps: 1, duration: 1, frames: 1, hasAlpha: false, hasAudio: false, isStill: false, kind: 'animation', premultiplied: false },
};

function tab() {
  const t: OpenedTab & { url: string; closed: boolean } = {
    url: '',
    closed: false,
    go(url) {
      this.url = url;
    },
    close() {
      this.closed = true;
    },
  };
  return t;
}

describe('editAsSource', () => {
  it('opens the tab first, then navigates it to /?src=<hash>', async () => {
    const t = tab();
    const order: string[] = [];
    const fromResult = vi.fn(async (recipeHash: string, name: string) => {
      order.push(`post ${recipeHash} ${name}`);
      return src;
    });
    const out = await editAsSource('r1', { name: 'out.gif' }, {
      openTab: () => {
        order.push('open');
        return t;
      },
      fromResult,
    });
    expect(order).toEqual(['open', 'post r1 out.gif']);
    expect(out).toEqual({ ok: true, source: src, url: `/?src=${HASH}` });
    expect(t.url).toBe(`/?src=${HASH}`);
    expect(t.closed).toBe(false);
  });

  it('closes the tab and reports the error when the server refuses', async () => {
    const t = tab();
    const out = await editAsSource('r1', { name: 'x.gif' }, {
      openTab: () => t,
      fromResult: async () => {
        throw new Error('no result for this recipe');
      },
    });
    expect(out).toEqual({ ok: false, error: 'no result for this recipe' });
    expect(t.closed).toBe(true);
    expect(t.url).toBe('');
  });

  it('reports a blocked pop-up with the URL to open by hand', async () => {
    const out = await editAsSource('r1', { name: 'x.gif' }, { openTab: () => null, fromResult: async () => src });
    expect(out).toEqual({ ok: true, source: src, url: `/?src=${HASH}`, blocked: true });
  });
});
