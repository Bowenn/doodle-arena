// Gradle downloads the Java 21 toolchain (build.gradle.kts) where it isn't installed.
plugins {
	id("org.gradle.toolchains.foojay-resolver-convention") version "1.0.0"
}

rootProject.name = "analytics"
