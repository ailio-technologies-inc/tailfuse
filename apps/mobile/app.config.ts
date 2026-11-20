import type { ConfigContext, ExpoConfig } from "expo/config";

// app.json holds the config. This adds the Apple team that signs the iOS app and its tunnel from
// the environment, so the open-source repo carries no account details:
//   APPLE_TEAM_ID=TEAMID1234 npx expo prebuild --platform ios --clean
// or put APPLE_TEAM_ID in .env.local (git-ignored; the Expo CLI reads it).
export default ({ config }: ConfigContext): ExpoConfig => {
  const appleTeamId = process.env.APPLE_TEAM_ID?.trim();
  return {
    ...config,
    ios: { ...config.ios, ...(appleTeamId ? { appleTeamId } : {}) },
  } as ExpoConfig;
};
