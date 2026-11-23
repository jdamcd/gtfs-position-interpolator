import { decodeFeed, fetchFeed } from "./feed.js";

const MAX_AGE = 15 * 1000;
const PER_PLATFORM = 3;
const GONE = new Set(["CANCELED", "DELETED"]);
const UNTIMED = new Set(["SKIPPED", "NO_DATA"]);

// A trip's last listed stop is its destination, not an arrival. Realtime
// trip IDs needn't match static ones, so there may be no headsign.
function indexCalls(feed) {
  const calls = new Map();
  for (const { tripUpdate } of feed.entity || []) {
    if (!tripUpdate || GONE.has(tripUpdate.trip?.scheduleRelationship)) continue;
    const updates = tripUpdate.stopTimeUpdate || [];
    const destination = updates.at(-1)?.stopId;
    for (const update of updates.slice(0, -1)) {
      // A trip's first stop may have only a departure time.
      const time = update.arrival?.time || update.departure?.time;
      if (!time || !update.stopId || UNTIMED.has(update.scheduleRelationship)) continue;
      if (!calls.has(update.stopId)) calls.set(update.stopId, []);
      calls.get(update.stopId).push({ route: tripUpdate.trip.routeId, destination, time });
    }
  }
  return calls;
}

export function createArrivals(urls) {
  const feeds = urls.map((url) => ({ url, calls: null, fetched: -Infinity, pending: null }));

  async function load(feed) {
    try {
      feed.calls = indexCalls(decodeFeed(await fetchFeed(feed.url)));
    } catch (err) {
      console.warn(`Trip updates from ${feed.url}: ${err.message}`);
    } finally {
      feed.fetched = performance.now();
      feed.pending = null;
    }
  }

  return {
    refresh() {
      const now = performance.now();
      return Promise.all(feeds.map((feed) => {
        if (!feed.pending && now - feed.fetched > MAX_AGE) feed.pending = load(feed);
        return feed.pending;
      }));
    },

    status() {
      if (feeds.some((feed) => feed.calls)) return "ready";
      return feeds.every((feed) => feed.fetched > -Infinity) ? "failed" : "loading";
    },

    at(platform, nowSeconds, routes) {
      return feeds.flatMap((feed) => feed.calls?.get(platform) ?? [])
        .filter(({ route, time }) => time >= nowSeconds && (routes.size === 0 || routes.has(route)))
        .sort((a, b) => a.time - b.time)
        .slice(0, PER_PLATFORM);
    },
  };
}
