export type ChangePayload = {
  /** "state": anything the state request reports changed. "domains": the set of owned names changed. */
  kind: string;
};

export type TailfuseEngineEvents = {
  change: (payload: ChangePayload) => void;
};

/** What the native module (Swift on iOS, Kotlin on Android) must implement under the name "TailfuseEngine". */
export interface TailfuseEngineNative {
  /** Asks for the VPN permission if needed, then turns Tailfuse on. */
  start(): Promise<void>;
  stop(): Promise<void>;
  isUp(): Promise<boolean>;
  /** One request, one response, both JSON as in engines/mobile/API.md. Never rejects for an engine error. */
  call(request: string): Promise<string>;
  getAlwaysOn(): Promise<boolean>;
  setAlwaysOn(enabled: boolean): Promise<void>;
  addListener(event: "change", listener: (payload: ChangePayload) => void): { remove(): void };
}
