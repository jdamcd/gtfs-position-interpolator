import { cumulative, metresFrom, pointAt, slice } from "./geo.js";

// Where a track's lines change, each eases to its new place over this
// distance in steps, rather than jumping.
const TAPER_METRES = 60;
const TAPER_STEPS = 6;
// Turns at a junction are judged this far along each track, and this far
// sideways counts as fully turned.
const LOOK_METRES = 60;
const FULL_TURN_METRES = 10;
// A crossing costs this much more than a line on the wrong side of a branch.
const CROSSING_COST = 10;

// Tracks meet where their ends share a point.
export function prepareTracks(tracks) {
  const prepared = tracks.map(({ line }) => {
    const distances = cumulative(line);
    const length = distances.at(-1);
    const look = Math.min(LOOK_METRES, length);
    const ends = [line[0], line.at(-1)].map((at, last) => ({
      key: at.join(","),
      into: metresFrom(at, pointAt(line, distances, last ? length - look : look)),
      neighbours: [],
    }));
    return { line, distances, length, taper: Math.min(TAPER_METRES, length / 2), ends };
  });

  const meetings = new Map();
  prepared.forEach((track, index) => track.ends.forEach((end, last) => {
    if (!meetings.has(end.key)) meetings.set(end.key, []);
    meetings.get(end.key).push({ index, last, end });
  }));
  for (const members of meetings.values()) {
    for (const a of members) {
      for (const b of members) {
        if (a === b) continue;
        const [ax, ay] = a.end.into;
        const [bx, by] = b.end.into;
        // b carries on from a if they leave the junction in opposite ways.
        const through = ax * bx + ay * by < 0;
        // How far b bears right of the way through a, in metres.
        const turn = (ax * by - ay * bx) / (Math.hypot(ax, ay) || 1);
        a.end.neighbours.push({
          index: b.index,
          last: b.last,
          through,
          // line-offset is to the right of each track's own direction.
          flip: (a.last === b.last ? -1 : 1) * (through ? 1 : -1),
          // In terms of a's offsets.
          side: (a.last ? 1 : -1) * Math.max(-1, Math.min(1, turn / FULL_TURN_METRES)),
        });
      }
    }
  }
  return prepared;
}

// Lines keep their places from one track into the next, and a line that
// turns off sits on that side, so it peels away without crossing the others.
export function orderTracks(prepared, colors) {
  const orders = colors.map((cs) => [...cs].sort());
  // A swap only reorders that pair, so only its crossings and sides change.
  const swapCost = (index, left, right) => {
    let change = 0;
    for (const { neighbours } of prepared[index].ends) {
      for (const n of neighbours) {
        if (!n.through) continue;
        const [l, r] = [orders[n.index].indexOf(left), orders[n.index].indexOf(right)];
        if (l >= 0 && r >= 0) change += CROSSING_COST * Math.sign(n.flip * (r - l));
        else if (l >= 0) change -= n.side;
        else if (r >= 0) change += n.side;
      }
    }
    return change;
  };
  for (let pass = 0, improved = true; improved && pass < 20; pass++) {
    improved = false;
    orders.forEach((order, index) => {
      for (let i = 0; i + 1 < order.length; i++) {
        if (swapCost(index, order[i], order[i + 1]) < 0) {
          [order[i], order[i + 1]] = [order[i + 1], order[i]];
          improved = true;
        }
      }
    });
  }
  return orders;
}

export function layoutTracks(prepared, colors) {
  const orders = orderTracks(prepared, colors);
  const offsetOf = (index, color) => orders[index].indexOf(color) - (orders[index].length - 1) / 2;
  const pieces = [];
  prepared.forEach((track, index) => {
    const cut = (from, to) => slice(track.line, track.distances, from, to);
    for (const color of orders[index]) {
      const offset = offsetOf(index, color);
      // Lines meeting at a junction ease to the average of where they are.
      const targets = track.ends.map(({ neighbours }) => {
        const others = neighbours.filter((n) => orders[n.index].includes(color)).map((n) => n.flip * offsetOf(n.index, color));
        return others.reduce((total, other) => total + other, offset) / (others.length + 1);
      });
      const eases = targets.map((target) => Math.abs(target - offset) > 0.01);
      pieces.push({ color, offset, line: cut(eases[0] ? track.taper : 0, eases[1] ? track.length - track.taper : track.length) });
      targets.forEach((target, last) => {
        if (!eases[last]) return;
        for (let k = 0; k < TAPER_STEPS; k++) {
          const [near, far] = [k, k + 1].map((step) => (step * track.taper) / TAPER_STEPS);
          const line = last ? cut(track.length - far, track.length - near) : cut(near, far);
          pieces.push({ color, offset: target + ((offset - target) * (k + 0.5)) / TAPER_STEPS, line });
        }
      });
    }
  });
  return pieces;
}
