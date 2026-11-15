import type { EventSubscription } from "expo-modules-core";

import type { ChangePayload } from "./src/TailfuseEngine.types";
import Native from "./src/TailfuseEngineModule";

export type { ChangePayload, TailfuseEngineNative } from "./src/TailfuseEngine.types";

/** True when the real engine is linked (a development build), false in Expo Go and on the web. */
export const isAvailable = Native != null;

function need() {
  if (!Native) throw new Error("the tailfuse engine is not available in this build");
  return Native;
}

export const start = (): Promise<void> => need().start();
export const stop = (): Promise<void> => need().stop();
export const isUp = (): Promise<boolean> => need().isUp();
export const call = (request: string): Promise<string> => need().call(request);
export const getAlwaysOn = (): Promise<boolean> => need().getAlwaysOn();
export const setAlwaysOn = (enabled: boolean): Promise<void> => need().setAlwaysOn(enabled);

/** Subscribes to the `change` event; returns a function that unsubscribes. A no-op without the module. */
export function addChangeListener(listener: (payload: ChangePayload) => void): () => void {
  if (!Native) return () => {};
  const sub: EventSubscription = Native.addListener("change", listener);
  return () => sub.remove();
}
