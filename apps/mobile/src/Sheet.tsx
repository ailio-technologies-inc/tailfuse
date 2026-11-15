// The one bottom sheet every secondary thing uses, now the platform's own (UISheetPresentationController on
// iOS, a Material bottom sheet on Android) through @lodev09/react-native-true-sheet: native grabber, corners,
// dimming, detents, swipe to dismiss and keyboard handling. Sheet.web.tsx is the browser preview's stand-in.
import { TrueSheet } from "@lodev09/react-native-true-sheet";
import { useEffect, useRef, type ReactNode, type RefObject } from "react";
import { Platform, ScrollView, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { CloseIcon, IconButton, T } from "./components";
import { colors } from "./theme";

// A ref to the list component (any scrollable); typed loosely because the sheet only passes it on.
type ListRef = RefObject<any>;

type Props = {
  visible: boolean;
  onClose: () => void;
  title?: string;
  subtitle?: ReactNode;
  /** Medium and large detents (long or growing content); otherwise the sheet fits its content. */
  tall?: boolean;
  fit?: boolean;
  /**
   * A virtualised list that is the sheet's one scroller (instead of wrapping children in a ScrollView): the
   * sheet wires the list's native scroll view for nested scrolling, so dragging the list scrolls it and
   * dragging at its top moves the sheet. Never nest it in another vertical ScrollView.
   */
  list?: (ref: ListRef) => ReactNode;
  children?: ReactNode;
};

export function Sheet({ visible, onClose, title, subtitle, tall, list, children }: Props) {
  // Android draws the sheet edge to edge, and a sheet that fits its content does not scroll, so nothing
  // insets it: with button navigation the last control sat under the bar. Pad for the bar ourselves there.
  const insets = useSafeAreaInsets();
  const padForBar = Platform.OS === "android" && !list;
  const sheet = useRef<TrueSheet>(null);
  const scroll = useRef<any>(null);
  const shown = useRef(false);
  const want = useRef(visible);
  const closeRef = useRef(onClose);
  useEffect(() => {
    closeRef.current = onClose;
    want.current = visible;
  });

  useEffect(() => {
    if (visible && !shown.current) {
      shown.current = true;
      sheet.current?.present().catch(() => {
        shown.current = false;
      });
    } else if (!visible && shown.current) {
      shown.current = false;
      sheet.current?.dismiss().catch(() => {});
    }
  }, [visible]);

  return (
    <TrueSheet
      ref={sheet}
      name={title}
      detents={tall ? [0.6, 1] : ["auto"]}
      backgroundColor={colors.card}
      grabber
      scrollableRef={scroll}
      scrollableOptions={padForBar ? { contentInsetAdjustment: "never" } : undefined}
      onDidDismiss={() => {
        const wasShown = shown.current;
        shown.current = false;
        // A swipe or a tap outside closes it natively: tell the screen. A close it asked for needs no word.
        if (wasShown || want.current) closeRef.current();
      }}
      header={
        <View style={{ flexDirection: "row", alignItems: "flex-start", gap: 8, paddingLeft: 20, paddingRight: 8, paddingTop: 18 }}>
          <View style={{ flex: 1, minWidth: 0, gap: 2, paddingVertical: 6 }}>
            {title ? <T v="h2" accessibilityRole="header" style={{ fontSize: 19, lineHeight: 24, letterSpacing: -0.3 }}>{title}</T> : null}
            {subtitle}
          </View>
          <IconButton onPress={() => sheet.current?.dismiss().catch(() => {})} label="Close">
            <CloseIcon size={18} color={colors.ink} />
          </IconButton>
        </View>
      }
    >
      {list ? (
        <View style={{ flex: 1 }}>{list(scroll)}</View>
      ) : (
      <ScrollView
        ref={scroll}
        nestedScrollEnabled
        contentContainerStyle={{ paddingHorizontal: 20, paddingTop: 8, paddingBottom: 28 + (padForBar ? insets.bottom : 0), gap: 16 }}
        keyboardShouldPersistTaps="handled"
        keyboardDismissMode="interactive"
        showsVerticalScrollIndicator={false}
      >
        {children}
      </ScrollView>
      )}
    </TrueSheet>
  );
}
