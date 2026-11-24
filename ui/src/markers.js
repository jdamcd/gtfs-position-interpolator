const LABEL_FONT = '700 11px system-ui, -apple-system, "Segoe UI", sans-serif';
const DIAMOND_RADIUS = 11.5;
// Room beyond a diamond for the selected ring and the shadow.
const DIAMOND_MARGIN = 8;

function canvas(width, height) {
  const element = document.createElement("canvas");
  element.width = width * 2;
  element.height = height * 2;
  const context = element.getContext("2d");
  context.scale(2, 2);
  return context;
}

// The label is drawn into the marker so overlapping markers hide each
// other's labels. As map text, every label would sit on top of every marker.
function pill(textWidth) {
  const width = Math.max(20, textWidth + 5);
  return {
    width: width + 12,
    height: 32,
    trace: (context, grow) => context.roundRect(6 - grow, 6 - grow, width + 2 * grow, 20 + 2 * grow, 10 + grow),
  };
}

function diamond(textWidth) {
  // Wide enough for the label to clear the diamond's sides.
  const radius = Math.max(DIAMOND_RADIUS, textWidth / 2 + 6);
  const size = 2 * Math.ceil(radius + DIAMOND_MARGIN);
  const c = size / 2;
  return {
    width: size,
    height: size,
    trace: (context, grow) => {
      const r = radius + grow * Math.SQRT2;
      context.moveTo(c, c - r);
      context.lineTo(c + r, c);
      context.lineTo(c, c + r);
      context.lineTo(c - r, c);
      context.closePath();
    },
  };
}

export function markerImage(color, textColor, label, shape, selected) {
  const measure = canvas(1, 1);
  measure.font = LABEL_FONT;
  const { width, height, trace } = (shape === "diamond" ? diamond : pill)(Math.ceil(measure.measureText(label).width));
  const context = canvas(width, height);
  const outline = (grow, fill) => {
    context.beginPath();
    trace(context, grow);
    context.fillStyle = fill;
    context.fill();
  };
  if (selected) {
    outline(4.5, "#111");
    outline(2, "#fff");
  } else {
    context.shadowColor = "rgb(0 0 0 / 0.45)";
    context.shadowBlur = 3;
    context.shadowOffsetY = 2;
    outline(1.5, "#fff");
    context.shadowColor = "transparent";
  }
  outline(0, `#${color}`);
  context.font = LABEL_FONT;
  context.fillStyle = `#${textColor}`;
  context.textAlign = "center";
  context.textBaseline = "middle";
  context.fillText(label, width / 2, height / 2 + 0.5);
  return context;
}

export function arrowImage(color) {
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
