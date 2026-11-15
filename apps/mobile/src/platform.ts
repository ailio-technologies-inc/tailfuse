import { Platform } from "react-native";

// "iPhone" or "iPad" on iOS, "phone" on Android and anywhere else.
export function deviceWord(): string {
  if (Platform.OS === "ios") return Platform.isPad ? "iPad" : "iPhone";
  return "phone";
}

// Whose account a purchase belongs to.
export function accountWord(): string {
  return Platform.OS === "android" ? "Google account" : "Apple ID";
}
