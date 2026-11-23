export function metresFrom([lon0, lat0], [lon, lat]) {
  const perDegree = 111_195;
  return [(lon - lon0) * perDegree * Math.cos((lat0 * Math.PI) / 180), (lat - lat0) * perDegree];
}

export function distance(a, b) {
  return Math.hypot(...metresFrom(a, b));
}

export function cumulative(line) {
  const distances = [0];
  for (let i = 1; i < line.length; i++) distances.push(distances[i - 1] + distance(line[i - 1], line[i]));
  return distances;
}

// The first index whose distance no longer comes before.
function search(distances, before) {
  let [low, high] = [0, distances.length];
  while (low < high) {
    const middle = (low + high) >> 1;
    if (before(distances[middle])) low = middle + 1;
    else high = middle;
  }
  return low;
}

export function pointAt(line, distances, metres) {
  const i = Math.min(Math.max(search(distances, (d) => d < metres), 1), line.length - 1);
  const span = distances[i] - distances[i - 1];
  const t = span ? Math.max(0, Math.min(1, (metres - distances[i - 1]) / span)) : 0;
  const [a, b] = [line[i - 1], line[i]];
  return [a[0] + t * (b[0] - a[0]), a[1] + t * (b[1] - a[1])];
}

export function slice(line, distances, from, to) {
  const inside = line.slice(search(distances, (d) => d <= from), search(distances, (d) => d < to));
  return [pointAt(line, distances, from), ...inside, pointAt(line, distances, to)];
}

// How far at is from the segment p–q in metres, and how far along it the
// nearest point is, as a fraction.
export function projectOnSegment(at, p, q) {
  const [ax, ay] = metresFrom(at, p);
  const [bx, by] = metresFrom(at, q);
  const [dx, dy] = [bx - ax, by - ay];
  const squared = dx * dx + dy * dy;
  const t = squared ? Math.max(0, Math.min(1, -(ax * dx + ay * dy) / squared)) : 0;
  return { away: Math.hypot(ax + t * dx, ay + t * dy), t };
}

export function nearestOnLine(line, at) {
  let best = null;
  for (let i = 1; i < line.length; i++) {
    const [p, q] = [line[i - 1], line[i]];
    const { away, t } = projectOnSegment(at, p, q);
    if (!best || away < best.away) best = { away, point: [p[0] + t * (q[0] - p[0]), p[1] + t * (q[1] - p[1])] };
  }
  return best;
}
