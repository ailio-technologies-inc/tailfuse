# gomobile's Java classes are reached from native code (JNI); R8 must not rename or strip them.
-keep class go.** { *; }
-keep class tfmobile.** { *; }
