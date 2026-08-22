// Server-side render of the header: the logo is a real link back to the
// landing page (keyboard reachable; resetApp / resetRender run on click).
import { render } from 'svelte/server';
import { describe, expect, it } from 'vitest';
import Header from './Header.svelte';

describe('Header (SSR)', () => {
  it('renders the GIF logo as a link to "/" with an accessible name', () => {
    const out = render(Header, { props: {} }).body;
    const logo = out.match(/<a[^>]*class="logo[^"]*"[^>]*>GIF<\/a>/)?.[0] ?? '';
    expect(logo).toBeTruthy();
    expect(logo).toContain('href="/"');
    expect(logo).toMatch(/aria-label="[^"]*start over[^"]*"/);
    expect(logo).toMatch(/title="[^"]*empty page[^"]*"/);
    // no aria-hidden decoration any more — it is the home control
    expect(logo).not.toContain('aria-hidden');
  });
});
