import mapboxgl from "mapbox-gl";
import { createArrivals } from "./arrivals.js";

const REFRESH_INTERVAL = 2500;

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

function formatWait(seconds) {
  return seconds < 60 ? "Due" : `${Math.round(seconds / 60)} min`;
}

// Feeds are fetched only while the popup is open and the page is visible.
export function createStationPopup(map, { urls, station, stopName, stopLabel, paintBadge, routes }) {
  const arrivals = urls.length ? createArrivals(urls) : null;
  const popup = new mapboxgl.Popup({ closeButton: false, closeOnClick: false, offset: 8, maxWidth: "280px" });
  let hovered = null;
  let pinned = null;
  let shown = null;
  let timer = null;

  function arrivalRow({ route, destination, time }, now) {
    const badge = element("span", "badge");
    paintBadge(badge, route);
    const row = element("li");
    row.append(
      badge,
      element("span", "destination", stopName(destination) || destination || "?"),
      element("span", "wait", formatWait(time - now)),
    );
    return row;
  }

  function sections({ platforms }) {
    const status = arrivals.status();
    if (status !== "ready") return [element("p", "popup-note", status === "loading" ? "Loading…" : "Arrivals unavailable")];
    const now = Date.now() / 1000;
    const wanted = new Set(routes());
    const lists = platforms
      .map((platform) => arrivals.at(platform, now, wanted))
      .filter((calls) => calls.length)
      .map((calls) => {
        const list = element("ul", "arrivals");
        list.append(...calls.map((call) => arrivalRow(call, now)));
        return list;
      });
    return lists.length ? lists : [element("p", "popup-note", "No arrivals")];
  }

  function render() {
    const at = shown && station(shown);
    if (!at) {
      popup.remove();
      return;
    }
    const content = element("div", "station-popup");
    content.append(element("h3", null, stopLabel(at.id)));
    if (arrivals) content.append(...sections(at));
    popup.setLngLat(at.at).setDOMContent(content);
    if (!popup.isOpen()) popup.addTo(map);
  }

  async function update() {
    if (document.hidden) return;
    await arrivals.refresh();
    render();
  }

  // Stations gone from the map are let go, so nothing is fetched for a
  // popup that can't show.
  function redraw() {
    const drawn = (id) => (id && station(id) ? id : null);
    hovered = drawn(hovered);
    pinned = drawn(pinned);
    const id = hovered ?? pinned;
    if (id !== shown) {
      shown = id;
      clearInterval(timer);
      timer = null;
      if (id && arrivals) {
        update();
        timer = setInterval(update, REFRESH_INTERVAL);
      }
    }
    render();
  }

  map.on("mousemove", "stations", ({ features: [feature] }) => {
    if (feature.properties.id === hovered) return;
    hovered = feature.properties.id;
    redraw();
  });
  map.on("mouseleave", "stations", () => {
    hovered = null;
    redraw();
  });
  document.addEventListener("visibilitychange", () => {
    if (timer) update();
  });

  return {
    pin(id) {
      pinned = id;
      redraw();
    },
    redraw,
  };
}
