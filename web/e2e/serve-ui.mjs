import { readFile } from "node:fs/promises";
import { createServer } from "node:http";

// Match the embedded server's /gitone/assets URLs and application-shell routes.
// Serving the production build also tests CSS before the React module loads.
const dist = new URL("../dist/", import.meta.url);
createServer(async (request, response) => {
  const path = new URL(request.url, "http://127.0.0.1").pathname;
  const asset = /^\/gitone\/assets\/([a-zA-Z0-9._-]+)$/.exec(path);
  const file = asset ? `assets/${asset[1]}` : "index.html";
  try {
    const content = await readFile(new URL(file, dist));
    const type = file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : "text/html";
    response.writeHead(200, { "Content-Type": `${type}; charset=utf-8`, "Cache-Control": "no-store" });
    response.end(content);
  } catch {
    response.writeHead(404);
    response.end();
  }
}).listen(4175, "127.0.0.1");
