# Evaluated by @bacons/apple-targets inside `target 'tunnel' do ... end` at the end of the generated
# ios/Podfile (paths are relative to ios/). Links the Go engine framework into the extension, and
# nothing else: no React Native, no Expo modules, so the extension stays well under its 50 MB limit.
pod 'TailfuseTfmobile', :path => '../modules/tailfuse-engine/ios/Frameworks'
