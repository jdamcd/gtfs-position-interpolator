import { readFile } from "node:fs/promises";
import { parse } from "csv-parse/sync";
import { strFromU8, unzipSync } from "fflate";

const TABLES = ["routes.txt", "trips.txt", "shapes.txt", "stops.txt"];

// About a metre, so that routes sharing track share points.
const round = (value) => Math.round(Number(value) * 1e5) / 1e5;

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

// Joins stations that shapes stop short of. The cap is the server's
// maxStopOffset.
const JOIN_METRES = 20;
const MAX_JOIN_METRES = 300;

function metresFrom([lon0, lat0], [lon, lat]) {
  const perDegree = 111_195;
  return [(lon - lon0) * perDegree * Math.cos((lat0 * Math.PI) / 180), (lat - lat0) * perDegree];
}

function nearestOnTracks(tracks, at) {
  let best = null;
  for (const track of tracks) {
    for (let i = 1; i < track.line.length; i++) {
      const [p, q] = [track.line[i - 1], track.line[i]];
      const [ax, ay] = metresFrom(at, p);
      const [bx, by] = metresFrom(at, q);
      const [dx, dy] = [bx - ax, by - ay];
      const length = dx * dx + dy * dy;
      const t = length ? Math.max(0, Math.min(1, -(ax * dx + ay * dy) / length)) : 0;
      const distance = Math.hypot(ax + t * dx, ay + t * dy);
      if (!best || distance < best.distance) {
        best = { distance, track, point: [round(p[0] + t * (q[0] - p[0])), round(p[1] + t * (q[1] - p[1]))] };
      }
    }
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

  const shapeRoutes = new Map();
  for (const row of table("trips.txt")) {
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
  const tracks = [];
  const drawn = new Set();
  for (const points of shapes.values()) {
    let run = null;
    for (let i = 1; i < points.length; i++) {
      const key = segmentKey(points[i - 1], points[i]);
      if (drawn.has(key) || segmentKey(points[i - 1], points[i - 1]) === key) {
        run = null;
        continue;
      }
      drawn.add(key);
      const label = JSON.stringify([...segmentRoutes.get(key)].sort());
      if (run?.label !== label) {
        run = { label, line: [points[i - 1]] };
        tracks.push(run);
      }
      run.line.push(points[i]);
    }
  }

  const stops = {};
  const stations = [];
  const bounds = [[Infinity, Infinity], [-Infinity, -Infinity]];
  for (const row of table("stops.txt")) {
    if (row.stop_name) stops[row.stop_id] = row.stop_name;
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

  const joins = [];
  for (const { at } of stations) {
    const nearest = nearestOnTracks(tracks, at);
    if (nearest && nearest.distance > JOIN_METRES && nearest.distance <= MAX_JOIN_METRES) {
      joins.push({ label: nearest.track.label, line: [nearest.point, at] });
    }
  }
  tracks.push(...joins);

  return { bounds, routes, stops, stations, tracks: tracks.map(({ label, line }) => ({ routes: JSON.parse(label), line })) };
}
