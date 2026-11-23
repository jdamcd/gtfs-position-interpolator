const LABEL_FONT = '700 11px system-ui, -apple-system, "Segoe UI", sans-serif';

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
export function markerImage(color, textColor, label, selected) {
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
