import { useEffect, useState, type ComponentRef, type ReactNode, type Ref } from "react";
import {
  AccessibilityInfo,
  ActivityIndicator,
  Animated,
  Pressable,
  Text,
  TextInput,
  View,
  type StyleProp,
  type TextInputProps,
  type TextProps,
  type TextStyle,
  type ViewStyle,
} from "react-native";
import Svg, { Circle, Path, Rect } from "react-native-svg";

import * as haptics from "./haptics";
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
const stroke = { fill: "none", strokeLinecap: "round", strokeLinejoin: "round" } as const;

function Icon({ d, size: s = 18, color = colors.muted, strokeWidth = 2, children }: IconProps & { d?: string; children?: ReactNode }) {
  return (
    <Svg viewBox="0 0 24 24" width={s} height={s} accessible={false}>
      {d ? <Path d={d} stroke={color} strokeWidth={strokeWidth} {...stroke} /> : null}
      {children}
    </Svg>
  );
}

export const TrashIcon = (p: IconProps) => <Icon {...p} d="M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3" />;
export const CheckIcon = (p: IconProps) => <Icon {...p} d="M5 12l5 5 9-10" />;
export const PlusIcon = ({ color = colors.accentInk, strokeWidth = 2.2, ...p }: IconProps) => <Icon {...p} color={color} strokeWidth={strokeWidth} d="M12 5v14M5 12h14" />;
export const CloseIcon = (p: IconProps) => <Icon {...p} d="M6 6l12 12M18 6L6 18" />;
export const ChevronIcon = ({ direction = "right", ...p }: IconProps & { direction?: "right" | "down" }) => (
  <Svg viewBox="0 0 24 24" width={p.size ?? 18} height={p.size ?? 18} accessible={false} style={{ transform: [{ rotate: direction === "down" ? "90deg" : "0deg" }] }}>
    <Path d="M9 6l6 6-6 6" stroke={p.color ?? colors.muted} strokeWidth={p.strokeWidth ?? 2} {...stroke} />
  </Svg>
);
export const GearIcon = ({ color = colors.ink, size: s = 22, strokeWidth = 1.8 }: IconProps) => (
  <Svg viewBox="0 0 24 24" width={s} height={s} accessible={false}>
    <Circle cx={12} cy={12} r={3} stroke={color} strokeWidth={strokeWidth} fill="none" />
    <Path
      d="M12 3v2.2M12 18.8V21M3 12h2.2M18.8 12H21M5.6 5.6l1.6 1.6M16.8 16.8l1.6 1.6M18.4 5.6l-1.6 1.6M7.2 16.8l-1.6 1.6"
      stroke={color}
      strokeWidth={strokeWidth}
      {...stroke}
    />
  </Svg>
);
export const SearchIcon = (p: IconProps) => (
  <Icon {...p} d="M20 20l-4.2-4.2">
    <Circle cx={11} cy={11} r={6.5} stroke={p.color ?? colors.muted} strokeWidth={p.strokeWidth ?? 2} fill="none" />
  </Icon>
);
// A port: a box with an arrow into it.
export const PortIcon = (p: IconProps) => <Icon {...p} d="M4 12h9M10 8l4 4-4 4M15 5h4a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1h-4" />;
export const CopyIcon = (p: IconProps) => (
  <Icon {...p} d="M5 15V6a1 1 0 0 1 1-1h9">
    <Rect x={9} y={9} width={11} height={11} rx={1} stroke={p.color ?? colors.muted} strokeWidth={p.strokeWidth ?? 2} fill="none" />
  </Icon>
);

// ---------- controls ----------

type SwitchProps = {
  value: boolean;
  onValueChange: (next: boolean) => void;
  label: string;
  small?: boolean;
  disabled?: boolean;
};

// The desktop's switch: 42x24 track, 16 knob (small: 36x20, 14), ok-green when on. A 44pt touch target.
export function Switch({ value, onValueChange, label, small, disabled }: SwitchProps) {
  const reduce = useReduceMotion();
  const w = small ? 36 : 42;
  const h = small ? 20 : 24;
  const knob = small ? 14 : 16;
  const inset = small ? 2 : 3;
  const travel = w - 2 - knob - inset * 2; // inside the 1px border
  const [x] = useState(() => new Animated.Value(value ? travel : 0));
  useEffect(() => {
    if (reduce) x.setValue(value ? travel : 0);
    else Animated.timing(x, { toValue: value ? travel : 0, duration: 160, useNativeDriver: true }).start();
  }, [value, reduce, travel, x]);

  return (
    <Pressable
      accessibilityRole="switch"
      accessibilityLabel={label}
      accessibilityState={{ checked: value, disabled: !!disabled }}
      disabled={disabled}
      onPress={() => {
        haptics.light();
        onValueChange(!value);
      }}
      hitSlop={{ top: (MIN_TOUCH - h) / 2, bottom: (MIN_TOUCH - h) / 2, left: 6, right: 6 }}
      style={({ pressed }) => ({ opacity: disabled ? 0.5 : pressed ? 0.7 : 1 })}
    >
      <View
        style={{
          width: w,
          height: h,
          borderRadius: radius.pill,
          borderWidth: 1,
          borderColor: value ? colors.ok : colors.lineStrong,
          backgroundColor: value ? colors.ok : colors.card,
          justifyContent: "center",
        }}
      >
        <Animated.View
          style={{
            width: knob,
            height: knob,
            borderRadius: knob / 2,
            marginLeft: inset - 1,
            backgroundColor: value ? (small ? "#ffffff" : colors.deep) : colors.muted,
            transform: [{ translateX: x }],
          }}
        />
      </View>
    </Pressable>
  );
}

type ButtonProps = {
  title: string;
  onPress: () => void;
  variant?: "primary" | "secondary";
  armed?: boolean;
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
export function Button({ title, onPress, variant = "secondary", armed, disabled, busy, block, label, expanded, checked, style }: ButtonProps) {
  const primary = variant === "primary";
  const off = !!disabled || !!busy;
  const fg = primary ? colors.accentInk : armed ? colors.bad : colors.ink;
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
          borderWidth: armed ? 1 : 0,
          borderColor: colors.badBorder,
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

export function IconButton({ onPress, label, armed, children }: { onPress: () => void; label: string; armed?: boolean; children: ReactNode }) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityState={{ selected: !!armed }}
      onPress={onPress}
      style={({ pressed }) => ({
        width: MIN_TOUCH,
        height: MIN_TOUCH,
        borderRadius: radius.pill,
        alignItems: "center",
        justifyContent: "center",
        backgroundColor: armed ? colors.badFillArmed : pressed ? colors.secondaryFill : "transparent",
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
      style={({ pressed }) => [
        { backgroundColor: pressed ? rowPressed : colors.card, borderWidth: 1, borderColor: pressed ? colors.lineStrong : colors.line, borderRadius: radius.md },
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
  return <View style={[{ backgroundColor: colors.card, borderWidth: 1, borderColor: colors.line, borderRadius: radius.md }, style]}>{children}</View>;
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
            borderColor: focus ? colors.accent : colors.lineStrong,
            borderRadius: radius.sm,
            outlineStyle: "none",
          } as unknown as TextStyle,
          style,
        ]}
      />
    </View>
  );
}
