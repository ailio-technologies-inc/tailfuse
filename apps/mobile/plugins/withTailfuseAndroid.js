// Expo config plugin for the Android side of tailfuse-engine (see modules/tailfuse-engine/android/README.md).
// Two edits the module cannot make itself:
//  1. android/app/build.gradle packages the engine's .aar (a library module may not bundle a local .aar).
//  2. the app's minSdk becomes 26, the level the .aar is built for (build-engine.sh passes -androidapi 26);
//     otherwise the manifest merger rejects the .aar's minSdkVersion.
// And one for looks: the app is always dark, but the theme follows the system, so on a phone in light mode
// with button navigation the bar was a light grey slab. The bar now stays clear with light buttons.
const { AndroidConfig, withAndroidStyles, withAppBuildGradle, withProjectBuildGradle } = require("expo/config-plugins");

const AAR = "../../modules/tailfuse-engine/android/libs/tfmobile.aar"; // relative to android/app
const APP_MARK = "// tailfuse-engine: packaged here";
const ROOT_MARK = "// tailfuse-engine: minSdk";

module.exports = function withTailfuseAndroid(config) {
  config = withAppBuildGradle(config, (c) => {
    let src = c.modResults.contents;
    if (!src.includes(APP_MARK)) {
      const line = `    ${APP_MARK}\n    implementation files("${AAR}")\n`;
      if (!/dependencies\s*\{/.test(src)) throw new Error("withTailfuseAndroid: no dependencies block in app/build.gradle");
      src = src.replace(/dependencies\s*\{\n/, (m) => m + line);
    }
    c.modResults.contents = src;
    return c;
  });
  config = withProjectBuildGradle(config, (c) => {
    let src = c.modResults.contents;
    if (!src.includes(ROOT_MARK)) {
      const anchor = /apply plugin: ["']expo-root-project["']/;
      if (!anchor.test(src)) {
        throw new Error('withTailfuseAndroid: could not find `apply plugin: "expo-root-project"` in android/build.gradle; set ext.minSdkVersion = 26 yourself');
      }
      // expo-root-project only sets minSdkVersion if it does not exist yet.
      src = src.replace(anchor, (m) => `${ROOT_MARK}\next { minSdkVersion = 26 }\n${m}`);
    }
    c.modResults.contents = src;
    return c;
  });
  config = withAndroidStyles(config, (c) => {
    const parent = { name: "AppTheme", parent: "Theme.AppCompat.DayNight.NoActionBar" };
    for (const [name, value] of [
      ["android:windowLightNavigationBar", "false"],
      ["android:windowLightStatusBar", "false"],
      ["android:enforceNavigationBarContrast", "false"],
    ]) {
      c.modResults = AndroidConfig.Styles.assignStylesValue(c.modResults, { add: true, parent, name, value, targetApi: "29" });
    }
    return c;
  });
  return config;
};
