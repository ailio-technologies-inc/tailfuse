require 'json'

# The Go engine framework alone, for the packet tunnel extension (targets/tunnel/pods.rb). The app
# gets the same framework through ../TailfuseEngine.podspec. This file sits one folder down on
# purpose: Expo autolinking links every podspec in the module's top-level folders and would add
# `import TailfuseTfmobile` (a module that does not exist) to the app's ExpoModulesProvider.
framework = File.join(__dir__, 'Tfmobile.xcframework')
unless File.directory?(framework)
  raise "TailfuseTfmobile: #{framework} is missing. Build the engine first: engines/mobile/scripts/build-engine.sh ios"
end

Pod::Spec.new do |s|
  s.name           = 'TailfuseTfmobile'
  s.version        = JSON.parse(File.read(File.join(__dir__, '..', '..', 'package.json')))['version']
  s.summary        = 'The Tailfuse Go engine (gomobile framework)'
  s.license        = { :type => 'Proprietary', :text => 'Copyright Ailio Technologies Inc.' }
  s.author         = 'Ailio Technologies Inc.'
  s.homepage       = 'https://tailfuse.com'
  s.platforms      = { :ios => '16.0' }
  s.source         = { git: 'https://github.com/ailio-technologies-inc/tailfuse.git' }
  s.static_framework = true

  s.vendored_frameworks = 'Tfmobile.xcframework'
  s.frameworks = 'Foundation', 'CoreFoundation', 'Security'
  s.libraries = 'resolv'
end
