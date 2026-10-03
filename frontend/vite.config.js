import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In dev, /api/* is proxied to the Go server so the browser sees a single origin (no CORS needed).
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": { target: "http://localhost:8080", rewrite: (p) => p.replace(/^\/api/, "") },
    },
  },
});
