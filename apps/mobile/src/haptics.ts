import * as Haptics from "expo-haptics";
import { Platform } from "react-native";

const on = Platform.OS !== "web";
const quiet = () => {};

export const light = () => (on ? Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light).catch(quiet) : undefined);
export const success = () => (on ? Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success).catch(quiet) : undefined);
export const warning = () => (on ? Haptics.notificationAsync(Haptics.NotificationFeedbackType.Warning).catch(quiet) : undefined);
