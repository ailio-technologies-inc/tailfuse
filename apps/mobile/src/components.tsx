import { useEffect, useState, type ComponentRef, type ReactNode, type Ref } from "react";
import {
  AccessibilityInfo,
  ActivityIndicator,
  Platform,
  Pressable,
  Switch as RNSwitch,
  Text,
  TextInput,
  View,
  type StyleProp,
  type TextInputProps,
  type TextProps,
  type TextStyle,
  type ViewStyle,
} from "react-native";
import Svg, { Circle, Rect } from "react-native-svg";

import { Icon } from "./Icon";
import { capList } from "./logic";
import { BTN_H, BTN_SLOP, MIN_TOUCH, colors, fonts, radius, rowPressed, size } from "./theme";

// ---------- motion ----------

export function useReduceMotion(): boolean {
  const [reduce, setReduce] = useState(false);
  useEffect(() => {
    let live = true;
    AccessibilityInfo.isReduceMotionEnabled().then((v) => live && setReduce(v)).catch(() => {});
    const sub = AccessibilityInfo.addEventListener("reduceMotionChanged", setReduce);
    return () => {
      live = false;
      sub.remove();
    };
  }, []);
  return reduce;
}

// ---------- text ----------

export type Variant = "label" | "small" | "body" | "status" | "h1" | "h2" | "mono";

const variants: Record<Variant, TextStyle> = {
  label: { fontFamily: fonts.regular, fontSize: size.label, lineHeight: size.label * 1.5 },
  small: { fontFamily: fonts.regular, fontSize: size.small, lineHeight: size.small * 1.5 },
  body: { fontFamily: fonts.regular, fontSize: size.body, lineHeight: size.body * 1.5 },
  status: { fontFamily: fonts.medium, fontSize: size.status, lineHeight: size.status * 1.25, letterSpacing: -0.6 },
  h1: { fontFamily: fonts.medium, fontSize: size.h1, lineHeight: size.h1 * 1.25, letterSpacing: -0.32 },
  h2: { fontFamily: fonts.medium, fontSize: size.h2, lineHeight: size.h2 * 1.3, letterSpacing: -0.22 },
  mono: { fontFamily: fonts.mono, fontSize: size.mono, lineHeight: size.mono * 1.5 },
};

type TProps = TextProps & { v?: Variant; color?: string; medium?: boolean };

// All text goes through here: set as text (never parsed), scales with Dynamic Type up to 1.6x.
export function T({ v = "small", color = colors.ink, medium, style, ...rest }: TProps) {
  return <Text maxFontSizeMultiplier={1.6} {...rest} style={[variants[v], medium && { fontFamily: fonts.medium }, { color }, style]} />;
}

// ---------- icons ----------

export function Mark({ width = 23 }: { width?: number }) {
  return (
    <Svg viewBox="120 146 272 220" width={width} height={(width * 220) / 272} accessible={false}>
      <Circle cx={150} cy={176} r={27} fill="#eff0f1" />
      <Circle cx={150} cy={256} r={27} fill="#eff0f1" />
      <Circle cx={150} cy={336} r={27} fill="#eff0f1" />
      <Circle cx={214} cy={176} r={27} fill="#eff0f1" />
      <Circle cx={214} cy={336} r={27} fill="#eff0f1" />
      <Circle cx={298} cy={176} r={27} fill="#ff4a1f" />
      <Circle cx={362} cy={176} r={27} fill="#ff4a1f" />
      <Circle cx={298} cy={336} r={27} fill="#ff4a1f" />
      <Circle cx={362} cy={256} r={27} fill="#ff4a1f" />
      <Circle cx={362} cy={336} r={27} fill="#ff4a1f" />
      <Rect x={191} y={229} width={130} height={54} rx={27} fill="#ff4a1f" />
    </Svg>
  );
}

type IconProps = { size?: number; color?: string; strokeWidth?: number };

export const TrashIcon = (p: IconProps) => <Icon name="trash" {...p} />;
export const CheckIcon = (p: IconProps) => <Icon name="check" weight="semibold" {...p} />;
export const PlusIcon = ({ color = colors.accentInk, ...p }: IconProps) => <Icon name="plus" weight="semibold" color={color} {...p} />;
export const CloseIcon = (p: IconProps) => <Icon name="close" {...p} />;
export const ChevronIcon = ({ direction = "right", ...p }: IconProps & { direction?: "right" | "down" }) => <Icon name={direction === "down" ? "chevron-down" : "chevron-right"} size={14} {...p} />;
export const GearIcon = ({ color = colors.ink, size: s = 22 }: IconProps) => <Icon name="gear" size={s} color={color} />;
export const SearchIcon = (p: IconProps) => <Icon name="search" {...p} />;
export const PortIcon = (p: IconProps) => <Icon name="port" {...p} />;
export const CopyIcon = (p: IconProps) => <Icon name="copy" {...p} />;
export const ScanIcon = (p: IconProps) => <Icon name="scan" {...p} />;

// ---------- controls ----------

type SwitchProps = {
  value: boolean;
  onValueChange: (next: boolean) => void;
  label: string;
  /** Kept for callers; the platform switch has one size. */
  small?: boolean;
  disabled?: boolean;
};

// The platform switch (UISwitch / Material), tinted: ok-green when on, graphite when off.
export function Switch({ value, onValueChange, label, disabled }: SwitchProps) {
  return (
    <RNSwitch
      accessibilityLabel={label}
      value={value}
      disabled={disabled}
      onValueChange={onValueChange}
      trackColor={{ false: colors.switchOff, true: colors.ok }}
      thumbColor={Platform.OS === "android" ? (value ? "#ffffff" : colors.muted) : undefined}
      ios_backgroundColor={colors.switchOff}
      style={{ opacity: disabled ? 0.5 : 1 }}
    />
  );
}

type ButtonProps = {
  title: string;
  onPress: () => void;
  variant?: "primary" | "secondary";
  /** Red text: removes something (the platform's destructive style). */
  destructive?: boolean;
  disabled?: boolean;
  /** Work in progress: shows a spinner and can't be tapped again. */
  busy?: boolean;
  /** Full width and 48 pt tall, for the one main action of a screen or sheet. */
  block?: boolean;
  label?: string;
  expanded?: boolean;
  /** Makes this a radio option instead of a plain button. */
  checked?: boolean;
  style?: StyleProp<ViewStyle>;
};

// Pill button. Primary: accent fill. Secondary: ink at 8%, no border. 36pt tall plus hitSlop = 44pt.
export function Button({ title, onPress, variant = "secondary", destructive, disabled, busy, block, label, expanded, checked, style }: ButtonProps) {
  const primary = variant === "primary";
  const off = !!disabled || !!busy;
  const fg = primary ? colors.accentInk : destructive ? colors.bad : colors.ink;
  return (
    <Pressable
      accessibilityRole={checked === undefined ? "button" : "radio"}
      accessibilityLabel={label ?? title}
      accessibilityState={{ disabled: off, busy: !!busy, ...(expanded === undefined ? {} : { expanded }), ...(checked === undefined ? {} : { checked }) }}
      disabled={off}
      onPress={onPress}
      hitSlop={BTN_SLOP + 2}
      style={({ pressed }) => [
        {
          height: block ? 48 : BTN_H,
          minWidth: MIN_TOUCH,
          paddingHorizontal: 16,
          borderRadius: radius.pill,
          flexDirection: "row",
          gap: 8,
          alignItems: "center",
          justifyContent: "center",
          alignSelf: block ? "stretch" : undefined,
          backgroundColor: primary ? colors.accent : pressed ? "rgba(239,240,241,0.14)" : colors.secondaryFill,
          opacity: disabled ? 0.5 : primary && pressed ? 0.8 : 1,
        },
        style,
      ]}
    >
      {busy ? <ActivityIndicator size="small" color={fg} /> : null}
      <T v={block ? "body" : "small"} medium color={fg} style={{ textAlign: "center" }}>
        {title}
      </T>
    </Pressable>
  );
}

export function LinkButton({ title, onPress, label }: { title: string; onPress: () => void; label?: string }) {
  return (
    <Pressable
      accessibilityRole="link"
      accessibilityLabel={label ?? title}
      onPress={onPress}
      hitSlop={{ top: 14, bottom: 14, left: 6, right: 6 }}
      style={({ pressed }) => ({ opacity: pressed ? 0.6 : 1 })}
    >
      <T v="small" color={colors.muted} style={{ textDecorationLine: "underline" }}>
        {title}
      </T>
    </Pressable>
  );
}

export function IconButton({ onPress, label, children }: { onPress: () => void; label: string; children: ReactNode }) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      onPress={onPress}
      android_ripple={{ color: "rgba(239,240,241,0.16)", radius: 24 }}
      style={({ pressed }) => ({
        width: MIN_TOUCH,
        height: MIN_TOUCH,
        borderRadius: radius.pill,
        alignItems: "center",
        justifyContent: "center",
        backgroundColor: pressed && Platform.OS !== "android" ? colors.secondaryFill : "transparent",
      })}
    >
      {children}
    </Pressable>
  );
}

// A tappable row/card with a lighter fill while pressed.
export function PressableCard({ onPress, label, hint, expanded, style, children }: {
  onPress: () => void;
  label: string;
  hint?: string;
  expanded?: boolean;
  style?: StyleProp<ViewStyle>;
  children: ReactNode;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityHint={hint}
      accessibilityState={expanded === undefined ? undefined : { expanded }}
      onPress={onPress}
      android_ripple={{ color: "rgba(239,240,241,0.10)" }}
      style={({ pressed }) => [
        { backgroundColor: pressed ? rowPressed : colors.card, borderRadius: radius.md, borderCurve: "continuous", overflow: "hidden" },
        style,
      ]}
    >
      {children}
    </Pressable>
  );
}

// ---------- layout ----------

export function Pane({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <View style={{ gap: 10 }}>
      <View style={{ flexDirection: "row", alignItems: "baseline", justifyContent: "space-between", flexWrap: "wrap", gap: 12 }}>
        <T v="h2" accessibilityRole="header">
          {title}
        </T>
        {hint ? <T v="label" color={colors.muted}>{hint}</T> : null}
      </View>
      {children}
    </View>
  );
}

export const Hint = ({ children, live }: { children: ReactNode; live?: boolean }) =>
  children ? (
    <T v="label" color={colors.muted} accessibilityLiveRegion={live ? "polite" : undefined} accessibilityRole={live ? "alert" : undefined}>
      {children}
    </T>
  ) : null;

export function Card({ children, style }: { children: ReactNode; style?: StyleProp<ViewStyle> }) {
  return <View style={[{ backgroundColor: colors.card, borderRadius: radius.md, borderCurve: "continuous", overflow: "hidden" }, style]}>{children}</View>;
}

export function Notice({ title, text, onDismiss }: { title: string; text: string; onDismiss: () => void }) {
  return (
    <View
      accessibilityRole="alert"
      accessibilityLiveRegion="assertive"
      style={{
        flexDirection: "row",
        flexWrap: "wrap",
        alignItems: "center",
        justifyContent: "space-between",
        gap: 12,
        padding: 14,
        borderWidth: 1,
        borderColor: colors.badBorder,
        borderRadius: radius.md,
        // desktop: bad at 9% over bg
        backgroundColor: "#2d2322",
      }}
    >
      <View style={{ flexGrow: 1, flexShrink: 1, flexBasis: 200, gap: 2 }}>
        <T v="small" medium color={colors.bad}>{title}</T>
        <T v="label" color={colors.muted}>{text}</T>
      </View>
      <Button title="Dismiss" onPress={onDismiss} />
    </View>
  );
}

export type InputRef = ComponentRef<typeof TextInput>;
type FieldProps = TextInputProps & { label: string; hideLabel?: boolean; grow?: boolean; ref?: Ref<InputRef> };

// A labelled input; mono like the desktop's. 48 pt tall.
export function Field({ label, hideLabel, grow, style, ...rest }: FieldProps) {
  const [focus, setFocus] = useState(false);
  return (
    <View style={{ gap: 4, minWidth: 0, flexGrow: grow ? 1 : 0, flexShrink: 1, flexBasis: grow ? 180 : "auto" }}>
      {hideLabel ? null : <T v="label" medium>{label}</T>}
      <TextInput
        accessibilityLabel={label}
        placeholderTextColor="rgba(156,158,163,0.75)"
        autoCapitalize="none"
        autoCorrect={false}
        spellCheck={false}
        selectionColor={colors.accent}
        keyboardAppearance="dark"
        clearButtonMode="while-editing"
        {...rest}
        onFocus={(e) => {
          setFocus(true);
          rest.onFocus?.(e);
        }}
        onBlur={(e) => {
          setFocus(false);
          rest.onBlur?.(e);
        }}
        style={[
          {
            minHeight: 48,
            paddingHorizontal: 14,
            paddingVertical: 0,
            fontFamily: fonts.mono,
            fontSize: 16, // 16 keeps iOS from zooming into the field
            color: colors.ink,
            backgroundColor: colors.deep,
            borderWidth: 1,
            borderColor: focus ? colors.accent : colors.line,
            borderRadius: radius.sm,
            borderCurve: "continuous",
            outlineStyle: "none",
          } as unknown as TextStyle,
          style,
        ]}
      />
    </View>
  );
}

// The platform's search field look: rounded, filled, a magnifier, a clear button, dark keyboard.
export function SearchField({ label, value, onChangeText, placeholder }: { label: string; value: string; onChangeText: (t: string) => void; placeholder: string }) {
  return (
    <View style={{ justifyContent: "center" }}>
      <TextInput
        accessibilityLabel={label}
        value={value}
        onChangeText={onChangeText}
        placeholder={placeholder}
        placeholderTextColor="rgba(156,158,163,0.85)"
        autoCapitalize="none"
        autoCorrect={false}
        spellCheck={false}
        returnKeyType="search"
        clearButtonMode="while-editing"
        keyboardAppearance="dark"
        selectionColor={colors.accent}
        style={
          {
            height: 40,
            paddingLeft: 36,
            paddingRight: 12,
            paddingVertical: 0,
            fontFamily: fonts.regular,
            fontSize: 16,
            color: colors.ink,
            backgroundColor: colors.secondaryFill,
            borderRadius: radius.sm,
            borderCurve: "continuous",
            outlineStyle: "none",
          } as unknown as TextStyle
        }
      />
      <View pointerEvents="none" style={{ position: "absolute", left: 11 }}>
        <SearchIcon size={16} />
      </View>
    </View>
  );
}

/** A tailnet's domains on one line: the first two, then "+N more" (a tailnet can own many). */
export function SuffixLine({ suffixes }: { suffixes: string[] }) {
  if (suffixes.length === 0) return null;
  const { shown, more } = capList(suffixes, 2);
  return (
    <T v="mono" color={colors.muted} numberOfLines={2}>
      {shown.map((x) => `*${x}`).join("  ")}
      {more ? `  ${more}` : ""}
    </T>
  );
}
