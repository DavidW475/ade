import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build ends up in internal/api/dist and is embedded into the binary via go:embed.
// In dev mode (npm run dev) Vite forwards /api, /auth and /ws to the daemon on :7000.
const daemon = {
  target: "http://127.0.0.1:7000",
  // The daemon checks host and origin (ADR-008).
  headers: { Host: "localhost:7000", Origin: "http://localhost:7000" },
};

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/api/dist",
    emptyOutDir: true,
    // Monaco (diff editor) is its own chunk of ~2.7 MB, loaded only on demand.
    chunkSizeWarningLimit: 3000,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: { "/api": daemon, "/auth": daemon, "/ws": { ...daemon, ws: true } },
  },
});
