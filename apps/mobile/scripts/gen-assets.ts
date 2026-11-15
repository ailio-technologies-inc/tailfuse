// Regenerates the app icon, adaptive icon, splash and favicon from ../../brand.
// Run: npx tsx scripts/gen-assets.ts
import { readFileSync } from "node:fs";
import { join } from "node:path";
import sharp from "sharp";

const brand = join(__dirname, "..", "..", "..", "brand");
const out = join(__dirname, "..", "assets");
const BG = "#18191b";

const square = readFileSync(join(brand, "app-icon-square.svg"));
const markSvg = readFileSync(join(brand, "mark.svg"), "utf8");
const markWhite = markSvg.replace(/#eff0f1|#ff4a1f/g, "#ffffff");

// The mark (272x220 viewBox) centred on a transparent square canvas, `width` px wide.
async function onCanvas(svg: string | Buffer, size: number, width: number) {
  const mark = await sharp(Buffer.from(svg), { density: 600 }).resize({ width }).png().toBuffer();
  return sharp({ create: { width: size, height: size, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
    .composite([{ input: mark, gravity: "center" }])
    .png();
}

async function main() {
  // iOS and the store: the full-bleed tile (the system rounds it).
  await sharp(square, { density: 600 }).resize(1024, 1024).flatten({ background: BG }).png().toFile(join(out, "icon.png"));
  // Android adaptive: the mark inside the 66% safe zone, graphite behind it.
  await (await onCanvas(markSvg, 1024, 520)).toFile(join(out, "android-icon-foreground.png"));
  await sharp({ create: { width: 1024, height: 1024, channels: 4, background: BG } }).png().toFile(join(out, "android-icon-background.png"));
  await (await onCanvas(markWhite, 1024, 520)).toFile(join(out, "android-icon-monochrome.png"));
  await (await onCanvas(markSvg, 1024, 560)).toFile(join(out, "splash-icon.png"));
  await sharp(square, { density: 300 }).resize(48, 48).png().toFile(join(out, "favicon.png"));
}
main();
