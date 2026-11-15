// The one-time "Unlimited tailnets" purchase. expo-iap needs a development build, so it is loaded
// only when its native module is linked; on the web and in Expo Go a small mock stands in.
import { requireOptionalNativeModule } from "expo";
import { Platform } from "react-native";

export const PRODUCT_ID = "com.ailio.tailfuse.unlimited";

type Iap = typeof import("expo-iap");

let iap: Iap | null = null;
if (Platform.OS !== "web" && requireOptionalNativeModule("ExpoIap")) {
  try {
    // eslint-disable-next-line @typescript-eslint/no-require-imports -- only loaded when the native module exists
    iap = require("expo-iap") as Iap;
  } catch {
    iap = null;
  }
}

/** True when the real store is reachable (a development build on a phone). */
export const storeAvailable = iap !== null;

export type PurchaseOutcome = "purchased" | "cancelled" | "pending";

// ----- mock (preview) -----
let mockOwned = typeof location !== "undefined" && new URLSearchParams(location.search).has("unlocked");
const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));

// ----- real store -----
let connected: Promise<unknown> | null = null;
const connect = () => (connected ??= iap!.initConnection().catch((e) => {
  connected = null;
  throw e;
}));

type Waiter = { resolve: (o: PurchaseOutcome) => void; reject: (e: Error) => void };
let waiter: Waiter | null = null;
let listening = false;
let onOwned: (() => void) | null = null;

// Finishes every transaction for our product the moment the store reports it, including ones that
// arrive outside a purchase (an Ask to Buy approval, a purchase made on another device).
function ensureListeners() {
  if (!iap || listening) return;
  listening = true;
  iap.purchaseUpdatedListener(async (purchase) => {
    if (purchase.productId !== PRODUCT_ID) return;
    if (purchase.purchaseState === "pending") {
      waiter?.resolve("pending");
      waiter = null;
      return;
    }
    try {
      // Acknowledges on Android (else Google refunds after 3 days) and clears iOS' queue.
      await iap!.finishTransaction({ purchase, isConsumable: false });
    } catch {
      // It stays queued and is finished at the next launch.
    }
    waiter?.resolve("purchased");
    waiter = null;
    onOwned?.();
  });
  iap.purchaseErrorListener((error) => {
    if (!waiter) return;
    if (iap!.isUserCancelledError(error)) waiter.resolve("cancelled");
    else waiter.reject(new Error(error.message || "purchase failed"));
    waiter = null;
  });
}

/** Called whenever a purchase completes outside purchase() (e.g. a deferred approval). */
export function onPurchaseCompleted(fn: () => void) {
  onOwned = fn;
  if (iap) {
    connect().then(ensureListeners).catch(() => {});
  }
}

/** The localised price from the store, e.g. "$4.99"; null when it can't be fetched. Never hard-coded. */
export async function getProduct(): Promise<{ id: string; price: string } | null> {
  if (!iap) {
    await wait(400);
    return { id: PRODUCT_ID, price: "$4.99" }; // preview only: there is no store to ask
  }
  await connect();
  const list = await iap.fetchProducts({ skus: [PRODUCT_ID], type: "in-app" });
  const p = (list ?? []).find((x) => x.id === PRODUCT_ID);
  return p ? { id: p.id, price: p.displayPrice } : null;
}

/** Whether the store records the purchase. Throws when the store can't be asked, so callers keep what they had. */
export async function isOwned(): Promise<boolean> {
  if (!iap) return mockOwned;
  await connect();
  ensureListeners();
  const purchases = await iap.getAvailablePurchases();
  const mine = purchases.filter((p) => p.productId === PRODUCT_ID && p.purchaseState === "purchased");
  for (const p of mine) {
    await iap.finishTransaction({ purchase: p, isConsumable: false }).catch(() => {});
  }
  return mine.length > 0;
}

export async function purchase(): Promise<PurchaseOutcome> {
  if (!iap) {
    await wait(1000);
    mockOwned = true;
    return "purchased";
  }
  await connect();
  ensureListeners();
  const result = new Promise<PurchaseOutcome>((resolve, reject) => (waiter = { resolve, reject }));
  try {
    await iap.requestPurchase({
      type: "in-app",
      request: { apple: { sku: PRODUCT_ID }, google: { skus: [PRODUCT_ID] } },
    });
  } catch (e) {
    waiter = null;
    if (iap.isUserCancelledError(e)) return "cancelled";
    throw e;
  }
  return result;
}

/** Asks the store to sync earlier purchases, then reports whether the unlock is among them. */
export async function restore(): Promise<boolean> {
  if (!iap) {
    await wait(700);
    return mockOwned;
  }
  await connect();
  await iap.restorePurchases();
  return isOwned();
}
