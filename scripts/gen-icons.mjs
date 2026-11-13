// Generates the tailfuse brand icons from the dealt palette + the three-node
// glyph (the header logo): a limewash rounded square with an ochre mark for
// the app icon, and a transparent template glyph for the menu-bar tray.
//
//   node scripts/gen-icons.mjs
//
// Pure Node (zlib + hand-rolled PNG chunks), no dependencies. After running,
// regenerate the bundle sizes with: npx tauri icon app/src-tauri/icons/icon.png
import { deflateSync } from 'node:zlib';
import { writeFileSync, mkdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const OUT = fileURLToPath(new URL('../app/src-tauri/icons/', import.meta.url));

// ---------------------------------------------------------------- PNG writer
const crcTable = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();
const crc32 = (buf) => {
  let c = 0xffffffff;
  for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
};
const chunk = (type, data) => {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
};
function png(width, height, rgba) {
  const raw = Buffer.alloc((width * 4 + 1) * height);
  for (let y = 0; y < height; y++) {
    raw[y * (width * 4 + 1)] = 0; // filter: none
    rgba.copy(raw, y * (width * 4 + 1) + 1, y * width * 4, (y + 1) * width * 4);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; ihdr[9] = 6; // 8-bit RGBA
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

// ---------------------------------------------------------------- drawing
const hex = (h) => [parseInt(h.slice(1, 3), 16), parseInt(h.slice(3, 5), 16), parseInt(h.slice(5, 7), 16)];
const LIMEWASH = hex('#f4efe5');
const OCHRE = hex('#8c6418');
const INK = hex('#29251e');

function canvas(size) {
  const px = Buffer.alloc(size * size * 4);
  return {
    px, size,
    set(x, y, [r, g, b], a = 1) {
      x |= 0; y |= 0;
      if (x < 0 || y < 0 || x >= size || y >= size) return;
      const i = (y * size + x) * 4;
      px[i] = r * a + px[i] * (1 - a);
      px[i + 1] = g * a + px[i + 1] * (1 - a);
      px[i + 2] = b * a + px[i + 2] * (1 - a);
      px[i + 3] = Math.min(255, px[i + 3] + a * 255 * (1 - px[i + 3] / 255));
    },
    alpha(x, y) {
      x |= 0; y |= 0;
      if (x < 0 || y < 0 || x >= size || y >= size) return 0;
      return this.px[(y * size + x) * 4 + 3] / 255;
    },
  };
}

/** the tailfuse glyph: one node left, two right, connected (the header logo) */
function drawGlyph(c, color, box, weight = 1) {
  const { size } = c;
  const [x0, y0, w, h] = box;
  const nodes = [
    [x0 + w * 0.16, y0 + h * 0.50],
    [x0 + w * 0.82, y0 + h * 0.16],
    [x0 + w * 0.82, y0 + h * 0.84],
  ];
  const r = w * 0.11 * weight;
  const lw = Math.max(1.5, w * 0.055 * weight);
  // connections (slight thickness, round joins via stamp of disks)
  for (const [a, b] of [[0, 1], [0, 2]]) {
    const [ax, ay] = nodes[a], [bx, by] = nodes[b];
    const steps = Math.ceil(Math.hypot(bx - ax, by - ay) * 2);
    for (let i = 0; i <= steps; i++) {
      const t = i / steps;
      disk(c, ax + (bx - ax) * t, ay + (by - ay) * t, lw / 2, color, 1);
    }
  }
  for (const [nx, ny] of nodes) disk(c, nx, ny, r, color, 1);
}

function disk(c, cx, cy, r, color, alpha = 1) {
  const R = Math.ceil(r + 1);
  for (let y = -R; y <= R; y++) {
    for (let x = -R; x <= R; x++) {
      const d = Math.hypot(x, y);
      const cov = Math.min(1, Math.max(0, r + 0.5 - d));
      if (cov > 0) c.set(cx + x, cy + y, color, alpha * cov);
    }
  }
}

// ---------------------------------------------------------------- app icon
{
  const S = 1024;
  const c = canvas(S);
  const radius = S * 0.225; // macOS squircle-ish corner
  for (let y = 0; y < S; y++) {
    for (let x = 0; x < S; x++) {
      // distance outside the rounded rect
      const dx = Math.max(0, Math.max(radius - x, x - (S - radius)));
      const dy = Math.max(0, Math.max(radius - y, y - (S - radius)));
      const d = Math.hypot(dx, dy);
      const cov = Math.min(1, Math.max(0, radius + 0.5 - d));
      if (cov > 0) c.set(x, y, LIMEWASH, cov);
    }
  }
  drawGlyph(c, OCHRE, [S * 0.22, S * 0.22, S * 0.56, S * 0.56]);
  mkdirSync(OUT, { recursive: true });
  writeFileSync(`${OUT}icon.png`, png(S, S, c.px));
  console.log('icon.png (app, 1024)');
}

// ---------------------------------------------------------------- tray icon
// Full-colour glyph (ochre on transparent), like Tailscale's own menu-bar
// icon: readable on both light and dark menu bars without template tinting.
for (const S of [18, 36]) {
  const c = canvas(S);
  drawGlyph(c, OCHRE, [S * 0.10, S * 0.10, S * 0.80, S * 0.80], S <= 18 ? 1.6 : 1.35);
  writeFileSync(`${OUT}tray-${S}.png`, png(S, S, c.px));
  console.log(`tray-${S}.png (menu-bar, full colour)`);
}
