import { defineConfig } from "vitest/config";

// The gateway embeds the build from internal/gateway/webui/dist and serves
// it under /ui/. `npm run dev` proxies the API to a gateway on :8080.
export default defineConfig({
  base: "/ui/",
  build: {
    outDir: "../internal/gateway/webui/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: { "/api": "http://127.0.0.1:8080" },
  },
  test: {
    environment: "jsdom",
    include: ["test/**/*.test.{ts,tsx}"],
  },
});
