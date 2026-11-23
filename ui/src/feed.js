import GtfsRealtimeBindings from "gtfs-realtime-bindings";

const { FeedMessage } = GtfsRealtimeBindings.transit_realtime;

export async function fetchFeed(url) {
  const response = await fetch(url, { cache: "no-store" });
  if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
  return new Uint8Array(await response.arrayBuffer());
}

export function decodeFeed(bytes) {
  return FeedMessage.toObject(FeedMessage.decode(bytes), { enums: String, longs: Number });
}
