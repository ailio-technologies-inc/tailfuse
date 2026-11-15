// Design tokens from the desktop window (app/src/styles.css), dark only.
import { StyleSheet } from "react-native";

export const colors = {
  bg: "#18191b",
  card: "#222427",
  deep: "#101112",
  ink: "#eff0f1",
  muted: "#9c9ea3",
  accent: "#ff4a1f",
  accentInk: "#18191b",
  ok: "#3ecf72",
  bad: "#ff8a6b",
  line: "rgba(239,240,241,0.11)",
  lineStrong: "rgba(239,240,241,0.20)",
  secondaryFill: "rgba(239,240,241,0.08)",
  badTint: "rgba(255,138,107,0.09)",
  badBorder: "rgba(255,138,107,0.45)",
  badFillArmed: "rgba(255,138,107,0.12)",
  accentTint: "rgba(255,74,31,0.06)",
  dot: "rgba(156,158,163,0.55)",
  backdrop: "rgba(8,9,10,0.6)",
} as const;

export const radius = { sm: 2, md: 0, lg: 0, pill: 999 } as const;

export const fonts = {
  regular: "HostGrotesk_400Regular",
  medium: "HostGrotesk_500Medium",
  mono: "JetBrainsMono_400Regular",
} as const;

export const size = { label: 13, small: 14, body: 16, status: 20, h2: 15, h1: 16, mono: 12 } as const;

export const MIN_TOUCH = 44;
// Visual height of the small desktop button; hitSlop brings the touch target to 44.
export const BTN_H = 36;
export const BTN_SLOP = (MIN_TOUCH - BTN_H) / 2;

export const hairline = StyleSheet.hairlineWidth;

// Content never stretches wider than this (iPad, web).
export const MAX_W = 560;
export const rowPressed = "#2a2c30";
