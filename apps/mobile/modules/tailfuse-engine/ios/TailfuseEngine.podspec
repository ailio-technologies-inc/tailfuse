require 'json'

package = JSON.parse(File.read(File.join(__dir__, '..', 'package.json')))

# The Go engine, built by gomobile (static: a Go c-archive inside a framework). It is not checked
# in; build it first. The same framework is linked into the packet tunnel extension through
# Frameworks/TailfuseTfmobile.podspec (see targets/tunnel/pods.rb).
framework = File.join(__dir__, 'Frameworks', 'Tfmobile.xcframework')
unless File.directory?(framework)
  raise "TailfuseEngine: #{framework} is missing. Build the engine first: engines/mobile/scripts/build-engine.sh ios"
end

Pod::Spec.new do |s|
  s.name           = 'TailfuseEngine'
  s.version        = package['version']
  s.summary        = 'The Tailfuse engine for the iOS app: VPN control and engine calls'
  s.description    = 'Turns the Tailfuse packet tunnel on and off and passes JSON requests to the engine.'
  s.license        = { :type => 'Proprietary', :text => 'Copyright Ailio Technologies Inc.' }
  s.author         = 'Ailio Technologies Inc.'
  s.homepage       = 'https://tailfuse.com'
  s.platforms      = {
    :ios => '16.4'
  }
  s.swift_version  = '5.9'
  s.source         = { git: 'https://github.com/ailio-technologies-inc/tailfuse.git' }
  s.static_framework = true

  s.dependency 'ExpoModulesCore'

  # Only this folder's Swift: a ** glob would also pick up the framework's headers.
  s.source_files = '*.swift'
  s.vendored_frameworks = 'Frameworks/Tfmobile.xcframework'
  # Link-tested: the archive needs Foundation, CoreFoundation and Security (it also names them in
  # its own linker options). libresolv is not referenced by the current build; it is listed so a
  # Go change to the cgo resolver cannot break the link.
  s.frameworks = 'NetworkExtension', 'Foundation', 'CoreFoundation', 'Security'
  s.libraries = 'resolv'

  s.pod_target_xcconfig = {
    'DEFINES_MODULE' => 'YES',
    'SWIFT_COMPILATION_MODE' => 'wholemodule'
  }
end
