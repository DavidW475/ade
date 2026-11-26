import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Der Build landet in internal/api/dist und wird per go:embed ins Binary eingebettet.
// Im Dev-Modus (npm run dev) leitet Vite /api, /auth und /ws an den Daemon auf :7000 weiter.
const daemon = {
  target: "http://127.0.0.1:7000",
  // Der Daemon prüft Host und Origin (ADR-008).
  headers: { Host: "localhost:7000", Origin: "http://localhost:7000" },
};

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/api/dist",
    emptyOutDir: true,
    // Monaco (Diff-Editor) ist ein eigener, erst bei Bedarf geladener Chunk von ~2,7 MB.
    chunkSizeWarningLimit: 3000,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: { "/api": daemon, "/auth": daemon, "/ws": { ...daemon, ws: true } },
  },
});
