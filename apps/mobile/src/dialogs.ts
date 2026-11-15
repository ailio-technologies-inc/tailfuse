// Native confirmations and choices: Alert on the phone, ActionSheetIOS on iOS, a plain browser
// prompt on the web preview.
import { ActionSheetIOS, Alert, Platform } from "react-native";

/** A destructive confirmation ("Remove tailnet"). Resolves true when the person confirms. */
export function confirmDestructive(title: string, message: string, action: string): Promise<boolean> {
  if (Platform.OS === "web") return Promise.resolve(typeof window !== "undefined" && window.confirm(`${title}\n\n${message}`));
  return new Promise((resolve) => {
    Alert.alert(
      title,
      message,
      [
        { text: "Cancel", style: "cancel", onPress: () => resolve(false) },
        { text: action, style: "destructive", onPress: () => resolve(true) },
      ],
      { cancelable: true, onDismiss: () => resolve(false) },
    );
  });
}

/** Picks one of `options` (an action sheet on iOS, an alert on Android). Resolves the index, or null when cancelled. */
export function choose(title: string, options: string[], message?: string): Promise<number | null> {
  if (Platform.OS === "web") {
    if (typeof window === "undefined") return Promise.resolve(null);
    const answer = window.prompt(`${title}\n${options.map((o, i) => `${i + 1}. ${o}`).join("\n")}`, "1");
    const n = Number(answer);
    return Promise.resolve(Number.isInteger(n) && n >= 1 && n <= options.length ? n - 1 : null);
  }
  if (Platform.OS === "ios") {
    return new Promise((resolve) => {
      ActionSheetIOS.showActionSheetWithOptions({ title, message, options: [...options, "Cancel"], cancelButtonIndex: options.length, userInterfaceStyle: "dark" }, (i) =>
        resolve(i === options.length ? null : i),
      );
    });
  }
  return new Promise((resolve) => {
    Alert.alert(title, message, [...options.map((o, i) => ({ text: o, onPress: () => resolve(i) })), { text: "Cancel", style: "cancel" as const, onPress: () => resolve(null) }], {
      cancelable: true,
      onDismiss: () => resolve(null),
    });
  });
}

/** iOS has a native one-line text prompt; elsewhere the caller shows its own field. */
export const hasNativePrompt = Platform.OS === "ios";

/** Asks for one line of text with a native prompt (iOS only). Resolves the text, or null when cancelled. */
export function promptText(title: string, message: string, action: string): Promise<string | null> {
  return new Promise((resolve) => {
    Alert.prompt(
      title,
      message,
      [
        { text: "Cancel", style: "cancel", onPress: () => resolve(null) },
        { text: action, onPress: (v?: string) => resolve(v ?? "") },
      ],
      "plain-text",
      "",
      "url",
    );
  });
}
