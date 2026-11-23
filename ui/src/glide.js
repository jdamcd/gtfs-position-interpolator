import { cumulative, distance, projectOnSegment, slice } from "./geo.js";

// A position further than this from its route's track glides straight.
const ON_TRACK_METRES = 20;
// A way along the track this much longer than the straight line has taken a
// wrong turn.
const MAX_DETOUR = 2;
// Track segments are found by grid cell. Cells are wider than ON_TRACK_METRES,
// so a point's own cell and those around it hold every segment near enough.
const CELL_DEGREES = 0.001;

const cell = (x, y) => x * 1e6 + y;

// Tracks are prepared by layout.js.
export function indexTracks(tracks, prepared) {
  const cells = new Map();
  prepared.forEach(({ line }, index) => {
    for (let i = 1; i < line.length; i++) {
      const [[x0, y0], [x1, y1]] = [line[i - 1], line[i]];
      for (let x = Math.floor(Math.min(x0, x1) / CELL_DEGREES); x <= Math.floor(Math.max(x0, x1) / CELL_DEGREES); x++) {
        for (let y = Math.floor(Math.min(y0, y1) / CELL_DEGREES); y <= Math.floor(Math.max(y0, y1) / CELL_DEGREES); y++) {
          if (!cells.has(cell(x, y))) cells.set(cell(x, y), []);
          cells.get(cell(x, y)).push([index, i]);
        }
      }
    }
  });
  return { tracks, prepared, cells };
}

function along(track, from, to) {
  return from <= to
    ? slice(track.line, track.distances, from, to)
    : slice(track.line, track.distances, to, from).reverse();
}

// The way along the route's track between two positions, following a train
// onto the next track where they meet, so it doesn't cut across curves.
export function glidePath({ tracks, prepared, cells }, route, from, to) {
  const straight = [from, to];
  if (from[0] === to[0] && from[1] === to[1]) return straight;
  const nearest = ([lon, lat]) => {
    const [x, y] = [Math.floor(lon / CELL_DEGREES), Math.floor(lat / CELL_DEGREES)];
    let best = null;
    for (let dx = -1; dx <= 1; dx++) {
      for (let dy = -1; dy <= 1; dy++) {
        for (const [index, i] of cells.get(cell(x + dx, y + dy)) ?? []) {
          if (!tracks[index].routes.includes(route)) continue;
          const { line, distances } = prepared[index];
          const { away, t } = projectOnSegment([lon, lat], line[i - 1], line[i]);
          if (!best || away < best.away) best = { index, away, metres: distances[i - 1] + t * (distances[i] - distances[i - 1]) };
        }
      }
    }
    return best;
  };
  const [a, b] = [nearest(from), nearest(to)];
  if (!a || !b || a.away > ON_TRACK_METRES || b.away > ON_TRACK_METRES) return straight;

  const [first, second] = [prepared[a.index], prepared[b.index]];
  const ways = a.index === b.index
    ? [along(first, a.metres, b.metres)]
    : first.ends.flatMap(({ neighbours }, last) => neighbours
      .filter((n) => n.index === b.index && n.through)
      .map((n) => [...along(first, a.metres, last ? first.length : 0), ...along(second, n.last ? second.length : 0, b.metres)]));
  const shortest = ways
    .map((way) => ({ way, length: cumulative(way).at(-1) }))
    .reduce((best, candidate) => (!best || candidate.length < best.length ? candidate : best), null);
  if (!shortest || shortest.length > MAX_DETOUR * distance(from, to) + ON_TRACK_METRES) return straight;
  return [from, ...shortest.way, to];
}
