import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Output goes into the Go package so `go build` embeds it.
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: { outDir: "../internal/web/dist", emptyOutDir: true, sourcemap: false },
  server: { proxy: { "/api": "http://127.0.0.1:8484" } },
});
