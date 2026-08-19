// Unit tests for the framework-free parts of src/lib (`npm test`). Reuses the
// Vite config (Svelte plugin, so `.svelte.ts` modules with runes compile) and
// runs in plain Node — no DOM emulation; browser-only APIs are injected by
// the code under test (see lib/still.ts).
import { defineConfig, mergeConfig } from 'vitest/config';
import viteConfig from './vite.config.ts';

export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      include: ['src/**/*.test.ts'],
      environment: 'node',
    },
  }),
);
