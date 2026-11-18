import { defineConfig, loadEnv } from "vite";
import { buildNetwork } from "./network.js";

const HOUR = 60 * 60 * 1000;
const RETRY = 5 * 60 * 1000;

// Serves network.json, built from the static GTFS feed, from the dev and
// preview servers, rebuilding it once it is older than the refresh period.
// It is also written into the production build, for hosts that only serve
// files.
function network(source, refresh) {
  let current = null;
  let building = null;

  async function load(logger) {
    if (!source) throw new Error("GTFS_STATIC_URL is not set. Add it to .env.local.");
    if (current && Date.now() < current.until) return current.json;
    building ??= buildNetwork(source).then(JSON.stringify).finally(() => { building = null; });
    try {
      current = { json: await building, until: Date.now() + refresh };
    } catch (err) {
      if (!current) throw err;
      logger.warn(`network.json: keeping the last copy, could not rebuild it: ${err.message}`);
      current.until = Date.now() + RETRY;
    }
    return current.json;
  }

  function serve(server) {
    server.middlewares.use("/network.json", async (request, response) => {
      try {
        const body = await load(server.config.logger);
        response.setHeader("Content-Type", "application/json");
        response.setHeader("Cache-Control", "no-cache");
        response.end(body);
      } catch (err) {
        server.config.logger.error(`network.json: ${err.message}`);
        response.statusCode = 500;
        response.setHeader("Content-Type", "text/plain");
        response.end(err.message);
      }
    });
  }

  return {
    name: "network",
    configureServer: serve,
    configurePreviewServer: serve,
    async generateBundle() {
      this.emitFile({ type: "asset", fileName: "network.json", source: await load() });
    },
  };
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, import.meta.dirname, "");
  const hours = Number(env.GTFS_STATIC_REFRESH_HOURS) || 24;
  return { plugins: [network(env.GTFS_STATIC_URL, hours * HOUR)] };
});
