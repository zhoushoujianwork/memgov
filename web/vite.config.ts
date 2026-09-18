import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { readFileSync } from "node:fs";

export default defineConfig({
  plugins: [
    react(),
    {
      name: "relayer-license",
      generateBundle() {
        this.emitFile({
          type: "asset",
          fileName: "assets/LICENSE-relayer.txt",
          source: readFileSync(
            new URL("./LICENSE-relayer", import.meta.url),
            "utf8",
          ),
        });
      },
    },
  ],
  base: "/",
  build: {
    outDir: "../internal/console/static",
    emptyOutDir: true,
    // The Go binary embeds the complete production output, never source files.
    assetsDir: "assets",
    target: "es2022",
  },
  server: {
    host: "127.0.0.1",
    port: 5178,
    strictPort: true,
    allowedHosts: ["localhost", "127.0.0.1"],
    proxy: {
      "/api/v1": {
        target: "http://127.0.0.1:8787",
        changeOrigin: true,
        configure(proxy) {
          proxy.on("proxyReq", (request, incoming) => {
            // Only this loopback development origin may use the existing controls.
            if (
              ["http://127.0.0.1:5178", "http://localhost:5178"].includes(
                incoming.headers.origin || "",
              )
            ) {
              request.setHeader("Origin", "http://127.0.0.1:8787");
            }
          });
        },
      },
    },
  },
});
