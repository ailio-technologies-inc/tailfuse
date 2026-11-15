import { requireOptionalNativeModule } from "expo";

import type { TailfuseEngineNative } from "./TailfuseEngine.types";

// Null where the native module is absent (web, Expo Go): the app then uses its in-memory mock.
export default requireOptionalNativeModule<TailfuseEngineNative>("TailfuseEngine");
