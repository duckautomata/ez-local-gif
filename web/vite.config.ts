import { existsSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { defineConfig, type Plugin } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// The Go binary embeds web/dist (see web/embed.go). Only dist/.gitkeep is
// committed and `go build` needs at least that file to exist, so instead of
// letting Vite empty the whole directory this plugin removes just the
// previous build output (no stale hashed assets end up in the binary) and
// makes sure the marker survives.
function cleanDist(outDir: string): Plugin {
  return {
    name: 'ezlg-clean-dist',
    apply: 'build',
    buildStart() {
      rmSync(resolve(outDir, 'assets'), { recursive: true, force: true });
      rmSync(resolve(outDir, 'index.html'), { force: true });
    },
    closeBundle() {
      mkdirSync(outDir, { recursive: true });
      const keep = resolve(outDir, '.gitkeep');
      if (!existsSync(keep)) writeFileSync(keep, '');
    },
  };
}

const outDir = resolve(import.meta.dirname, 'dist');

// Dev loop: `npm run dev` serves the SPA on :5173 and proxies the API to the
// Go server (`ezlg serve`, default :8080). Set EZLG_API to point elsewhere.
const apiTarget = process.env.EZLG_API ?? 'http://localhost:8080';

export default defineConfig({
  base: '/',
  plugins: [svelte(), cleanDist(outDir)],
  server: {
    port: 5173,
    strictPort: false,
    proxy: {
      '/api': { target: apiTarget, changeOrigin: false },
      '/out': { target: apiTarget, changeOrigin: false },
      '/healthz': { target: apiTarget, changeOrigin: false },
    },
  },
  build: {
    outDir,
    emptyOutDir: false, // handled by cleanDist so dist/.gitkeep is never removed
    assetsDir: 'assets',
    target: 'es2022',
    sourcemap: false,
    cssCodeSplit: false,
  },
});
