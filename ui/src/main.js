import GtfsRealtimeBindings from "gtfs-realtime-bindings";
import mapboxgl from "mapbox-gl";
import "mapbox-gl/dist/mapbox-gl.css";
import "./style.css";

const { FeedMessage } = GtfsRealtimeBindings.transit_realtime;
const POSITIONS_URL = import.meta.env.VITE_POSITIONS_URL;
const ROUTE_LABELS = Object.fromEntries((import.meta.env.VITE_ROUTE_LABELS || "").split(",").filter(Boolean).map((pair) => pair.split(":")));
const STATUSES = { INCOMING_AT: "arriving", IN_TRANSIT_TO: "in transit", STOPPED_AT: "stopped" };
const MAPBOX_TOKEN = import.meta.env.VITE_MAPBOX_TOKEN;
const POLL_INTERVAL = 5000;
const NETWORK_INTERVAL = 60 * 60 * 1000;
const DEFAULT_COLOR = "555555";
const DEFAULT_TEXT_COLOR = "FFFFFF";
// A train moving further than this between polls has been re-placed by the
// server, so it is shown jumping rather than gliding.
const MAX_GLIDE_METRES = 400;
const TRACK_WIDTH = 5;
const LABEL_FONT = '700 11px system-ui, -apple-system, "Segoe UI", sans-serif';
const DARK = matchMedia("(prefers-color-scheme: dark)").matches;
const EMPTY = { type: "FeatureCollection", features: [] };
const DEFAULT_ZOOM = 13;
const VIEW = viewFromUrl();

const $ = (id) => document.getElementById(id);

const vehicles = new Map();
let map = null;
let routes = {};
let stops = {};
let tracks = [];
let stations = [];
let framed = Boolean(VIEW.center);
let moved = false;
let smooth = $("smooth").checked;
let selectedId = null;
let inFlight = false;
let setupError = null;

function showError(message) {
  $("error").textContent = message;
  $("error").hidden = false;
}

function routeColor(route) {
  return routes[route]?.color || DEFAULT_COLOR;
}

function routeTextColor(route) {
  return routes[route]?.text_color || DEFAULT_TEXT_COLOR;
}

function routeLabel(route) {
  return ROUTE_LABELS[route] || routes[route]?.name || route || "?";
}

function stopLabel(id) {
  if (!id) return "?";
  return stops[id] ? `${stops[id]} (${id})` : id;
}

function selectedRoutes() {
  const ids = new Map(Object.keys(routes).map((id) => [id.toLowerCase(), id]));
  return $("routes").value.split(",").map((r) => r.trim()).filter(Boolean).map((r) => ids.get(r.toLowerCase()) || r);
}

function feedUrl() {
  const url = new URL(POSITIONS_URL, location.href);
  const wanted = selectedRoutes();
  if (wanted.length) url.searchParams.set("route", wanted.join(","));
  return url.href;
}

function canvas(width, height) {
  const element = document.createElement("canvas");
  element.width = width * 2;
  element.height = height * 2;
  const context = element.getContext("2d");
  context.scale(2, 2);
  return context;
}

// The label is drawn into the marker so that overlapping markers hide each
// other's labels. As separate map text, every label would sit on top of
// every marker.
function markerImage(color, textColor, label, selected) {
  const measure = canvas(1, 1);
  measure.font = LABEL_FONT;
  const width = Math.max(20, Math.ceil(measure.measureText(label).width) + 5);
  const context = canvas(width + 12, 32);
  const pill = (grow, fill) => {
    context.beginPath();
    context.roundRect(6 - grow, 6 - grow, width + 2 * grow, 20 + 2 * grow, 10 + grow);
    context.fillStyle = fill;
    context.fill();
  };
  if (selected) {
    pill(4.5, "#111");
    pill(2, "#fff");
  } else {
    context.shadowColor = "rgb(0 0 0 / 0.45)";
    context.shadowBlur = 3;
    context.shadowOffsetY = 2;
    pill(1.5, "#fff");
    context.shadowColor = "transparent";
  }
  pill(0, `#${color}`);
  context.font = LABEL_FONT;
  context.fillStyle = `#${textColor}`;
  context.textAlign = "center";
  context.textBaseline = "middle";
  context.fillText(label, (width + 12) / 2, 16.5);
  return context;
}

function arrowImage(color) {
  const context = canvas(14, 12);
  context.beginPath();
  context.moveTo(7, 1.5);
  context.lineTo(12.5, 10.5);
  context.lineTo(1.5, 10.5);
  context.closePath();
  context.lineJoin = "round";
  context.strokeStyle = "#fff";
  context.lineWidth = 2;
  context.stroke();
  context.fillStyle = `#${color}`;
  context.fill();
  return context;
}

function ensureImage(id, draw) {
  if (map.hasImage(id)) return id;
  const context = draw();
  const { width, height } = context.canvas;
  map.addImage(id, context.getImageData(0, 0, width, height), { pixelRatio: 2 });
  return id;
}

function addLayers() {
  const labels = map.getStyle().layers.find((layer) => layer.type === "symbol")?.id;
  for (const source of ["tracks", "stations", "vehicles"]) map.addSource(source, { type: "geojson", data: EMPTY });
  map.addLayer({
    id: "tracks",
    type: "line",
    source: "tracks",
    layout: { "line-cap": "round", "line-join": "round" },
    paint: {
      "line-color": ["get", "color"],
      "line-width": TRACK_WIDTH,
      "line-offset": ["get", "offset"],
      "line-opacity": 0.8,
    },
  }, labels);
  map.addLayer({
    id: "stations",
    type: "circle",
    source: "stations",
    minzoom: 11,
    paint: {
      "circle-radius": ["interpolate", ["linear"], ["zoom"], 11, 2.5, 14, 5.5, 17, 8],
      "circle-color": "#fff",
      "circle-stroke-color": "#333",
      "circle-stroke-width": ["interpolate", ["linear"], ["zoom"], 11, 1.5, 14, 2.5, 17, 3],
    },
  }, labels);
  map.addLayer({
    id: "headings",
    type: "symbol",
    source: "vehicles",
    filter: ["has", "bearing"],
    layout: {
      "icon-image": ["get", "arrow"],
      "icon-rotate": ["get", "bearing"],
      "icon-rotation-alignment": "map",
      "icon-offset": [0, -15],
      "icon-allow-overlap": true,
      "icon-ignore-placement": true,
    },
  });
  map.addLayer({
    id: "vehicles",
    type: "symbol",
    source: "vehicles",
    layout: {
      "icon-image": ["get", "icon"],
      "icon-allow-overlap": true,
      "icon-ignore-placement": true,
      "symbol-sort-key": ["case", ["get", "selected"], 1, 0],
    },
    paint: { "icon-opacity": ["case", ["get", "stopped"], 0.7, 1] },
  });
}

// Track shared by lines of different colours is drawn as parallel lines.
function drawTracks() {
  const wanted = new Set(selectedRoutes());
  const features = [];
  for (const track of tracks) {
    const colors = [...new Set(track.routes
      .filter((route) => wanted.size === 0 || wanted.has(route))
      .map(routeColor))].sort();
    colors.forEach((color, i) => features.push({
      type: "Feature",
      properties: { color: `#${color}`, offset: (i - (colors.length - 1) / 2) * TRACK_WIDTH },
      geometry: { type: "LineString", coordinates: track.line },
    }));
  }
  map.getSource("tracks").setData({ type: "FeatureCollection", features });
}

function drawStations() {
  map.getSource("stations").setData({
    type: "FeatureCollection",
    features: stations.map(({ id, at }) => ({
      type: "Feature",
      properties: { label: stopLabel(id) },
      geometry: { type: "Point", coordinates: at },
    })),
  });
}

// Trains glide to each new position over the time until the next poll, so
// the map runs one poll behind the feed.
function positionAt(vehicle, now) {
  if (!smooth) return vehicle.to;
  const t = Math.min(1, (now - vehicle.start) / POLL_INTERVAL);
  return [vehicle.from[0] + (vehicle.to[0] - vehicle.from[0]) * t, vehicle.from[1] + (vehicle.to[1] - vehicle.from[1]) * t];
}

function animate(now) {
  if (moved) {
    moved = false;
    const features = [];
    for (const vehicle of vehicles.values()) {
      if (smooth && now - vehicle.start < POLL_INTERVAL) moved = true;
      const selected = vehicle.properties.id === selectedId;
      features.push({
        type: "Feature",
        properties: { ...vehicle.properties, selected, icon: selected ? vehicle.selectedIcon : vehicle.icon },
        geometry: { type: "Point", coordinates: positionAt(vehicle, now) },
      });
    }
    map.getSource("vehicles").setData({ type: "FeatureCollection", features });
  }
  requestAnimationFrame(animate);
}

async function refresh() {
  if (inFlight) return;
  inFlight = true;
  const url = feedUrl();
  try {
    const started = performance.now();
    const response = await fetch(url, { cache: "no-store" });
    if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
    const bytes = new Uint8Array(await response.arrayBuffer());
    const elapsed = performance.now() - started;
    const feed = FeedMessage.toObject(FeedMessage.decode(bytes), { enums: String, longs: Number });
    render(feed);
    $("stat-url").textContent = `GET ${url}`;
    const age = Date.now() / 1000 - feed.header.timestamp;
    $("stat-response").textContent = `${(bytes.length / 1000).toFixed(1)} KB in ${elapsed.toFixed(0)} ms (${age.toFixed(1)} s old)`;
    if (setupError) showError(setupError);
    else $("error").hidden = true;
  } catch (err) {
    showError(`Request failed: ${err.message}`);
  } finally {
    inFlight = false;
  }
}

function render(feed) {
  const now = performance.now();
  const seen = new Set();
  const statusCounts = {};
  let selected = null;
  for (const entity of feed.entity || []) {
    const vehicle = entity.vehicle;
    if (!vehicle || !vehicle.position) continue;
    seen.add(entity.id);
    const status = vehicle.currentStatus || "IN_TRANSIT_TO";
    statusCounts[status] = (statusCounts[status] || 0) + 1;
    const to = [vehicle.position.longitude, vehicle.position.latitude];
    const drawn = vehicles.get(entity.id);
    // A tab in the background polls rarely, so the place a train was last
    // drawn is too old to glide from.
    const stale = !drawn || now - drawn.start > 2 * POLL_INTERVAL;
    let from = stale ? to : positionAt(drawn, now);
    if (new mapboxgl.LngLat(...from).distanceTo(new mapboxgl.LngLat(...to)) > MAX_GLIDE_METRES) from = to;
    const [color, textColor, label] = [routeColor, routeTextColor, routeLabel].map((of) => of(vehicle.trip.routeId));
    const marker = `marker ${color} ${textColor} ${label}`;
    const properties = { id: entity.id, stopped: status === "STOPPED_AT" };
    if (vehicle.position.bearing != null) {
      properties.bearing = vehicle.position.bearing;
      properties.arrow = ensureImage(`arrow ${color}`, () => arrowImage(color));
    }
    vehicles.set(entity.id, {
      from,
      to,
      start: now,
      properties,
      icon: ensureImage(marker, () => markerImage(color, textColor, label, false)),
      selectedIcon: ensureImage(`selected ${marker}`, () => markerImage(color, textColor, label, true)),
    });
    if (entity.id === selectedId) selected = { entity, headerTimestamp: feed.header.timestamp };
  }
  for (const id of vehicles.keys()) {
    if (!seen.has(id)) vehicles.delete(id);
  }
  moved = true;
  if (!framed && vehicles.size) {
    const bounds = new mapboxgl.LngLatBounds();
    for (const vehicle of vehicles.values()) bounds.extend(vehicle.to);
    frame(bounds);
  }
  $("stat-count").textContent = seen.size;
  $("stat-status").textContent = Object.entries(STATUSES)
    .map(([status, name]) => [statusCounts[status] || 0, name])
    .sort(([a], [b]) => b - a)
    .map(([count, name]) => `${count} ${name}`).join(", ");
  renderSelected(selected);
}

function renderSelected(selected) {
  $("selected").hidden = !selectedId;
  if (!selectedId) return;
  if (!selected) {
    $("sel-status").textContent = "no longer in the feed";
    return;
  }
  const vehicle = selected.entity.vehicle;
  const badge = $("sel-badge");
  badge.style.cssText = `--route:#${routeColor(vehicle.trip.routeId)};--route-text:#${routeTextColor(vehicle.trip.routeId)}`;
  badge.textContent = routeLabel(vehicle.trip.routeId);
  $("sel-trip").textContent = `${vehicle.trip.tripId} (${vehicle.trip.startDate})`;
  $("sel-id").textContent = selected.entity.id;
  $("sel-status").textContent = `${(vehicle.currentStatus || "IN_TRANSIT_TO").replaceAll("_", " ")} ${stopLabel(vehicle.stopId)}`;
  $("sel-sequence").textContent = vehicle.currentStopSequence ?? "not set";
  $("sel-position").textContent = `${vehicle.position.latitude.toFixed(6)}, ${vehicle.position.longitude.toFixed(6)}`;
  $("sel-bearing").textContent = vehicle.position.bearing == null ? "not set" : `${vehicle.position.bearing.toFixed(0)}°`;
  $("sel-timestamp").textContent = vehicle.timestamp
    ? `${vehicle.timestamp}, ${(selected.headerTimestamp - vehicle.timestamp).toFixed(0)} s before header`
    : "not set";
}

function select(id) {
  selectedId = id;
  moved = true;
  refresh();
}

function viewFromUrl() {
  const params = new URLSearchParams(location.search);
  const number = (name, min, max) => {
    const value = params.get(name)?.trim() ? Number(params.get(name)) : NaN;
    return value >= min && value <= max ? value : null;
  };
  const lat = number("lat", -90, 90);
  const lon = number("lon", -180, 180);
  return { center: lat !== null && lon !== null ? [lon, lat] : null, zoom: number("zoom", 0, 22) };
}

function frame(bounds) {
  framed = true;
  if (VIEW.center) return;
  const left = innerWidth > 800 ? 340 : 40;
  map.fitBounds(bounds, { padding: { top: 40, right: 40, bottom: 40, left }, animate: false });
  if (VIEW.zoom !== null) map.jumpTo({ center: mapboxgl.LngLatBounds.convert(bounds).getCenter(), zoom: VIEW.zoom });
}

async function loadNetwork() {
  try {
    const response = await fetch("network.json", { cache: "no-cache" });
    if (!response.ok) {
      const plain = response.headers.get("content-type")?.startsWith("text/plain");
      throw new Error(plain ? await response.text() : `${response.status} ${response.statusText}`);
    }
    const network = await response.json();
    ({ routes, stops, stations, tracks } = network);
    if (!framed) frame(network.bounds);
    drawTracks();
    drawStations();
    setupError = null;
  } catch (err) {
    // A failed reload leaves the network already on the map in place.
    if (tracks.length) return;
    setupError = `Could not load the network: ${err.message}`;
    showError(setupError);
  }
}

async function start() {
  if (!MAPBOX_TOKEN) {
    showError("VITE_MAPBOX_TOKEN is not set. Add a Mapbox public token to .env.local.");
    return;
  }
  map = new mapboxgl.Map({
    container: "map",
    accessToken: MAPBOX_TOKEN,
    style: `mapbox://styles/mapbox/${DARK ? "dark" : "light"}-v11`,
    ...(VIEW.center && { center: VIEW.center, zoom: VIEW.zoom ?? DEFAULT_ZOOM }),
  });
  map.addControl(new mapboxgl.NavigationControl({ showCompass: false }), "top-right");
  map.on("error", ({ error }) => showError(`Map error: ${error?.message || "unknown"}`));
  await map.once("load");
  addLayers();

  map.on("click", (event) => {
    const [hit] = map.queryRenderedFeatures(event.point, { layers: ["vehicles"] });
    if (hit) select(hit.properties.id);
    else if (selectedId) select(null);
  });
  map.on("mouseenter", "vehicles", () => { map.getCanvas().style.cursor = "pointer"; });
  map.on("mouseleave", "vehicles", () => { map.getCanvas().style.cursor = ""; });
  const stationName = new mapboxgl.Popup({ closeButton: false, closeOnClick: false, anchor: "bottom", offset: 8 });
  map.on("mousemove", "stations", ({ features: [station] }) => {
    stationName.setLngLat(station.geometry.coordinates).setText(station.properties.label).addTo(map);
  });
  map.on("mouseleave", "stations", () => stationName.remove());
  $("routes").addEventListener("change", () => {
    drawTracks();
    refresh();
  });
  $("smooth").addEventListener("change", (event) => {
    smooth = event.target.checked;
    // Turned back on mid-poll, trains wait where they are for the next
    // position rather than jumping back to finish the glide.
    for (const vehicle of vehicles.values()) vehicle.from = vehicle.to;
    moved = true;
  });

  await loadNetwork();
  setInterval(loadNetwork, NETWORK_INTERVAL);
  refresh();
  setInterval(refresh, POLL_INTERVAL);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) refresh();
  });
  requestAnimationFrame(animate);
}

start();
