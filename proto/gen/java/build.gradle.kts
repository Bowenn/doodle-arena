// Hand-written; `make proto` only replaces src/main/java.
// Runtime versions must match the plugin versions in proto/buf.gen.yaml.
plugins {
	`java-library`
}

group = "com.doodlearena"
version = "0.1.0"

java {
	toolchain {
		languageVersion = JavaLanguageVersion.of(25)
	}
}

repositories {
	mavenCentral()
}

dependencies {
	api("com.google.protobuf:protobuf-java:4.36.2")
	api("io.grpc:grpc-protobuf:1.85.0")
	api("io.grpc:grpc-stub:1.85.0")
	// javax.annotation.Generated, used by grpc-java's generated stubs on Java 9+.
	compileOnly("org.apache.tomcat:annotations-api:6.0.53")
}
