// Web preview only: the browser has no native sheet (Sheet.tsx uses TrueSheet on the phone). Hand-rolled (Animated + PanResponder): slides up
// over a backdrop, closes by drag-down, backdrop tap, the close button or Android's back button,
// falls back to a fade with reduce-motion, and keeps accessibility inside while it is open.
import { createRef, useEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { Animated, KeyboardAvoidingView, Modal, PanResponder, Platform, Pressable, ScrollView, StyleSheet, View, useWindowDimensions } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { CloseIcon, IconButton, T, useReduceMotion } from "./components";
import { MAX_W, colors, radius } from "./theme";

// A ref to the list component (any scrollable); typed loosely because the sheet only passes it on.
type ListRef = RefObject<any>;

type Props = {
  visible: boolean;
  onClose: () => void;
  title?: string;
  subtitle?: ReactNode;
  /** Medium and large detents (native) instead of fitting the content. */
  tall?: boolean;
  /** Native only: where it first opens. Ignored here. */
  fit?: boolean;
  /** Content that should not scroll with the rest, pinned under the header. */
  /** A virtualised list that is the sheet's one scroller (see Sheet.tsx). */
  list?: (ref: ListRef) => ReactNode;
  children?: ReactNode;
};

const native = Platform.OS !== "web";

const noRef = createRef<unknown>();

export function Sheet({ visible, onClose, title, subtitle, tall, list, children }: Props) {
  const insets = useSafeAreaInsets();
  const { height: screenH } = useWindowDimensions();
  const reduce = useReduceMotion();
  const [mounted, setMounted] = useState(visible);
  if (visible && !mounted) setMounted(true); // mount in the same render the sheet is asked for
  const [progress] = useState(() => new Animated.Value(visible ? 1 : 0));
  const [drag] = useState(() => new Animated.Value(0));
  const closeRef = useRef(onClose);
  useEffect(() => {
    closeRef.current = onClose;
  }, [onClose]);

  useEffect(() => {
    if (visible) {
      drag.setValue(0);
      Animated.timing(progress, { toValue: 1, duration: reduce ? 120 : 260, useNativeDriver: native }).start();
    } else {
      Animated.timing(progress, { toValue: 0, duration: reduce ? 100 : 200, useNativeDriver: native }).start(({ finished }) => {
        if (finished) setMounted(false);
      });
    }
  }, [visible, reduce, progress, drag]);

  // eslint-disable-next-line react-hooks/refs -- closeRef is only read in the gesture callback, never during render
  const [pan] = useState(() =>
    PanResponder.create({
      onMoveShouldSetPanResponder: (_e, g) => g.dy > 4 && Math.abs(g.dy) > Math.abs(g.dx),
      onPanResponderMove: (_e, g) => drag.setValue(Math.max(0, g.dy)),
      onPanResponderRelease: (_e, g) => {
        if (g.dy > 90 || g.vy > 0.9) closeRef.current();
        else Animated.spring(drag, { toValue: 0, useNativeDriver: native, bounciness: 0 }).start();
      },
      onPanResponderTerminate: () => Animated.spring(drag, { toValue: 0, useNativeDriver: native }).start(),
    }),
  );

  if (!mounted) return null;

  const maxH = (screenH - insets.top - 8) * (tall ? 0.9 : 0.85);
  const slide = progress.interpolate({ inputRange: [0, 1], outputRange: [reduce ? 0 : maxH, 0] });
  const translateY = Animated.add(slide, drag);

  return (
    <Modal visible transparent animationType="none" onRequestClose={onClose} statusBarTranslucent supportedOrientations={["portrait"]}>
      <View style={styles.root} accessibilityViewIsModal>
        <Animated.View style={[StyleSheet.absoluteFill, { backgroundColor: colors.backdrop, opacity: progress }]}>
          <Pressable style={StyleSheet.absoluteFill} onPress={onClose} accessibilityRole="button" accessibilityLabel="Close" />
        </Animated.View>
        <KeyboardAvoidingView behavior={Platform.OS === "ios" ? "padding" : "height"} style={{ maxHeight: screenH - insets.top, width: "100%", alignItems: "center" }} pointerEvents="box-none">
          <Animated.View
            accessibilityLabel={title}
            style={[styles.sheet, { maxHeight: maxH, opacity: reduce ? progress : 1, transform: [{ translateY }] }]}
          >
            <View {...pan.panHandlers}>
              <View style={styles.handleZone}>
                <View style={styles.handle} />
              </View>
              <View style={styles.header}>
                <View style={{ flex: 1, minWidth: 0, gap: 2, paddingVertical: 6 }}>
                  {title ? <T v="h2" accessibilityRole="header" style={{ fontSize: 19, lineHeight: 24, letterSpacing: -0.3 }}>{title}</T> : null}
                  {subtitle}
                </View>
                <IconButton onPress={onClose} label="Close">
                  <CloseIcon size={20} color={colors.ink} />
                </IconButton>
              </View>
            </View>
            {list ? (
              <View style={{ height: maxH - 84 }}>{list(noRef)}</View>
            ) : (
            <ScrollView
              style={{ flexShrink: 1 }}
              contentContainerStyle={{ paddingHorizontal: 20, paddingTop: 8, paddingBottom: 20 + insets.bottom, gap: 16 }}
              keyboardShouldPersistTaps="handled"
              keyboardDismissMode="interactive"
              showsVerticalScrollIndicator={false}
            >
              {children}
            </ScrollView>
            )}
          </Animated.View>
        </KeyboardAvoidingView>
      </View>
    </Modal>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, justifyContent: "flex-end" },
  sheet: {
    width: "100%",
    maxWidth: MAX_W,
    backgroundColor: colors.card,
    borderTopLeftRadius: radius.lg,
    borderTopRightRadius: radius.lg,
    overflow: "hidden",
  },
  handleZone: { height: 22, alignItems: "center", justifyContent: "flex-end", paddingBottom: 4 },
  handle: { width: 40, height: 4, borderRadius: 2, backgroundColor: colors.lineStrong },
  header: { flexDirection: "row", alignItems: "flex-start", gap: 8, paddingLeft: 20, paddingRight: 8 },
});
