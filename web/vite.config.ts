import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: '/',
  server: { proxy: { '/api': 'http://127.0.0.1:18851' } },
  build: {
    outDir: 'dist',
    // The Go embed placeholder is owned by the backend lane.
    emptyOutDir: false,
    assetsDir: 'assets',
  },
});
