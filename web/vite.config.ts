import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

function themeBootstrap(): Plugin {
  const sourcePath = new URL("./src/theme-init.js", import.meta.url);
  const source = readFileSync(sourcePath, "utf8");
  const hash = createHash("sha256").update(source).digest("hex").slice(0, 16);
  const fileName = `assets/theme-${hash}.js`;
  let base = "/gitone/";
  return {
    name: "gitone-theme-bootstrap",
    configResolved(config) {
      base = config.base;
    },
    configureServer(server) {
      server.middlewares.use(`${base}${fileName}`, (_request, response) => {
        response.setHeader("Content-Type", "text/javascript; charset=utf-8");
        response.setHeader("Cache-Control", "no-store");
        // Development reloads should see edits without restarting Vite.
        response.end(readFileSync(sourcePath, "utf8"));
      });
    },
    generateBundle() {
      // Content hashing matches the server's immutable /gitone/assets cache.
      this.emitFile({ type: "asset", fileName, source });
    },
    transformIndexHtml: {
      order: "post",
      handler: () => [{
        tag: "script",
        attrs: { src: `${base}${fileName}` },
        injectTo: "head",
      }],
    },
  };
}

export default defineConfig({ plugins: [react(), themeBootstrap()], base: "/gitone/" });
