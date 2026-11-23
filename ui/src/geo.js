export function metresFrom([lon0, lat0], [lon, lat]) {
  const perDegree = 111_195;
  return [(lon - lon0) * perDegree * Math.cos((lat0 * Math.PI) / 180), (lat - lat0) * perDegree];
}
