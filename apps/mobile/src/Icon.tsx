// One icon wrapper: SF Symbols on iOS (expo-symbols), Ionicons from @expo/vector-icons on Android
// and web. The Tailfuse mark stays SVG (components.tsx).
import Ionicons from "@expo/vector-icons/Ionicons";
import { SymbolView, type SymbolViewProps } from "expo-symbols";
import { Platform } from "react-native";

import { colors } from "./theme";

const icons = {
  gear: { sf: "gearshape", ion: "settings-outline" },
  trash: { sf: "trash", ion: "trash-outline" },
  check: { sf: "checkmark", ion: "checkmark" },
  plus: { sf: "plus", ion: "add" },
  close: { sf: "xmark", ion: "close" },
  "chevron-right": { sf: "chevron.right", ion: "chevron-forward" },
  "chevron-down": { sf: "chevron.down", ion: "chevron-down" },
  search: { sf: "magnifyingglass", ion: "search" },
  port: { sf: "arrow.right.to.line", ion: "arrow-forward-circle-outline" },
  copy: { sf: "doc.on.doc", ion: "copy-outline" },
  scan: { sf: "qrcode.viewfinder", ion: "qr-code-outline" },
  globe: { sf: "globe", ion: "globe-outline" },
} as const satisfies Record<string, { sf: SymbolViewProps["name"]; ion: keyof typeof Ionicons.glyphMap }>;

export type IconName = keyof typeof icons;

export function Icon({ name, size = 20, color = colors.muted, weight = "regular" }: { name: IconName; size?: number; color?: string; weight?: SymbolViewProps["weight"] }) {
  const i = icons[name];
  if (Platform.OS === "ios") return <SymbolView name={i.sf} size={size} tintColor={color} weight={weight} resizeMode="scaleAspectFit" style={{ width: size, height: size }} />;
  return <Ionicons name={i.ion} size={size} color={color} accessible={false} />;
}
