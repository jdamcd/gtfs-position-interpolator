import { readFile } from "node:fs/promises";
import { parse } from "csv-parse/sync";
import { strFromU8, unzipSync } from "fflate";
import { nearestOnLine } from "./src/geo.js";

const TABLES = ["routes.txt", "trips.txt", "shapes.txt", "stops.txt", "stop_times.txt"];

// About 10 cm: enough for shared track to share points, fine enough not to
// show as steps.
const round = (value) => Math.round(Number(value) * 1e6) / 1e6;

async function load(source) {
  if (!/^https?:/.test(source)) return readFile(source);
  const response = await fetch(source);
  if (!response.ok) throw new Error(`${source}: ${response.status} ${response.statusText}`);
  return new Uint8Array(await response.arrayBuffer());
}

function segmentKey(a, b) {
  const [p, q] = [a.join(","), b.join(",")].sort();
  return `${p}|${q}`;
}

// A branch can leave mid-run without changing its routes, so runs are also
// cut where others end.
function cutAtEnds(runs) {
  const ends = new Set(runs.flatMap(({ line }) => [line[0].join(), line.at(-1).join()]));
  return runs.flatMap(({ label, line }) => {
    const cuts = line.flatMap((point, i) => (i === 0 || i === line.length - 1 || ends.has(point.join()) ? [i] : []));
    return cuts.slice(1).map((to, i) => ({ label, line: line.slice(cuts[i], to + 1) }));
  });
}

// Joins stations that shapes stop short of. The cap is the server's
// maxStopOffset.
const JOIN_METRES = 20;
const MAX_JOIN_METRES = 300;

function nearestOnTracks(tracks, at) {
  let best = null;
  for (const track of tracks) {
    const nearest = nearestOnLine(track.line, at);
    if (!best || nearest.away < best.away) best = { ...nearest, track };
  }
  return best;
}

export async function buildNetwork(zipUrlOrPath) {
  const files = unzipSync(await load(zipUrlOrPath), { filter: (file) => TABLES.includes(file.name) });

  function table(name) {
    if (!files[name]) return [];
    return parse(strFromU8(files[name]), { columns: true, bom: true, trim: true, skip_empty_lines: true, relax_column_count: true });
  }

  const routes = {};
  for (const row of table("routes.txt")) {
    routes[row.route_id] = {
      name: row.route_short_name || row.route_long_name || row.route_id,
      color: row.route_color || null,
      text_color: row.route_text_color || null,
    };
  }

  const tripRoutes = new Map();
  const shapeRoutes = new Map();
  for (const row of table("trips.txt")) {
    tripRoutes.set(row.trip_id, row.route_id);
    if (!row.shape_id) continue;
    if (!shapeRoutes.has(row.shape_id)) shapeRoutes.set(row.shape_id, new Set());
    shapeRoutes.get(row.shape_id).add(row.route_id);
  }

  const shapes = new Map();
  for (const row of table("shapes.txt")) {
    if (!shapeRoutes.has(row.shape_id)) continue;
    if (!shapes.has(row.shape_id)) shapes.set(row.shape_id, []);
    shapes.get(row.shape_id).push([Number(row.shape_pt_sequence), round(row.shape_pt_lon), round(row.shape_pt_lat)]);
  }
  for (const [id, points] of shapes) {
    points.sort((a, b) => a[0] - b[0]);
    shapes.set(id, points.map(([, lon, lat]) => [lon, lat]));
  }

  const segmentRoutes = new Map();
  for (const [id, points] of shapes) {
    for (let i = 1; i < points.length; i++) {
      const key = segmentKey(points[i - 1], points[i]);
      if (!segmentRoutes.has(key)) segmentRoutes.set(key, new Set());
      for (const route of shapeRoutes.get(id)) segmentRoutes.get(key).add(route);
    }
  }

  // Each stretch of track is drawn once, in either direction, labelled with
  // every route whose shapes run along it.
  const runs = [];
  const drawn = new Set();
  for (const points of shapes.values()) {
    let run = null;
    for (let i = 1; i < points.length; i++) {
      const key = segmentKey(points[i - 1], points[i]);
      if (drawn.has(key) || points[i - 1].join() === points[i].join()) {
        run = null;
        continue;
      }
      drawn.add(key);
      const label = JSON.stringify([...segmentRoutes.get(key)].sort());
      if (run?.label !== label) {
        run = { label, line: [points[i - 1]] };
        runs.push(run);
      }
      run.line.push(points[i]);
    }
  }
  const tracks = cutAtEnds(runs);

  const stops = {};
  const parents = {};
  const platforms = new Map();
  const stations = [];
  const bounds = [[Infinity, Infinity], [-Infinity, -Infinity]];
  for (const row of table("stops.txt")) {
    if (row.stop_name) stops[row.stop_id] = row.stop_name;
    if (row.parent_station) parents[row.stop_id] = row.parent_station;
    if (row.parent_station && ["", "0"].includes(row.location_type ?? "")) {
      if (!platforms.has(row.parent_station)) platforms.set(row.parent_station, []);
      platforms.get(row.parent_station).push(row.stop_id);
    }
    const [lat, lon] = [Number(row.stop_lat), Number(row.stop_lon)];
    if (!row.stop_lat || !row.stop_lon || Number.isNaN(lat) || Number.isNaN(lon)) continue;
    // Platforms are left to their parent station, and entrances and the
    // like (location types 2 and up) are not places a vehicle stops.
    if (!row.parent_station && ["", "0", "1"].includes(row.location_type ?? "")) {
      stations.push({ id: row.stop_id, at: [round(lon), round(lat)] });
    }
    bounds[0] = [Math.min(bounds[0][0], lon), Math.min(bounds[0][1], lat)];
    bounds[1] = [Math.max(bounds[1][0], lon), Math.max(bounds[1][1], lat)];
  }
  if (bounds[0][0] === Infinity) throw new Error("stops.txt has no stops with coordinates");

  const stationRoutes = new Map();
  for (const row of table("stop_times.txt")) {
    const route = tripRoutes.get(row.trip_id);
    if (!route) continue;
    const station = parents[row.stop_id] || row.stop_id;
    if (!stationRoutes.has(station)) stationRoutes.set(station, new Set());
    stationRoutes.get(station).add(route);
  }
  for (const station of stations) {
    station.routes = [...(stationRoutes.get(station.id) ?? [])].sort();
    station.platforms = platforms.get(station.id) ?? [station.id];
  }

  const joins = [];
  for (const { at } of stations) {
    const nearest = nearestOnTracks(tracks, at);
    if (nearest && nearest.away > JOIN_METRES && nearest.away <= MAX_JOIN_METRES) {
      joins.push({ label: nearest.track.label, line: [nearest.point.map(round), at] });
    }
  }
  tracks.push(...joins);

  return { bounds, routes, stops, stations, tracks: tracks.map(({ label, line }) => ({ routes: JSON.parse(label), line })) };
}
