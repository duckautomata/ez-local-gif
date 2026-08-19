import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/vite-plugin-svelte').SvelteConfig} */
export default {
  // TypeScript inside <script lang="ts"> blocks.
  preprocess: vitePreprocess(),
  compilerOptions: {
    runes: true,
  },
};
