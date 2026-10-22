// The analytics service (ADR 0018): one Flink application, run in application mode on the
// official flink image. Flink itself is provided by the image (compileOnly); the job jar bundles
// what the image doesn't have: the Kafka connector, protobuf and the generated event classes.
plugins {
	java
	id("com.gradleup.shadow") version "9.6.1"
}

group = "com.doodlearena"
version = "0.1.0"
description = "Doodle Arena analytics: Flink jobs over the event streams (top guesses, wash trading)"

// The flink image runs Java 21 (Flink 2.2 supports 11, 17 and 21), so the job is built for it.
java {
	toolchain {
		languageVersion = JavaLanguageVersion.of(21)
	}
}

repositories {
	mavenCentral()
}

val flinkVersion = "2.2.1"
// The newest Kafka connector, built for Flink 2.2 (none supports 2.3 yet).
val kafkaConnectorVersion = "5.0.0-2.2"
// Keep equal to proto/gen/java/build.gradle.kts.
val protobufVersion = "4.36.2"

// The generated event classes (proto/gen/java) are compiled here at Java 21: proto/gen/java's
// own build targets Java 25 for the marketplace. Only the Kafka events and the game types they
// use; not the gRPC stubs.
val protoSources by tasks.registering(Sync::class) {
	from("../../proto/gen/java/src/main/java") {
		include("com/doodlearena/proto/doodle/events/v1/**")
		include("com/doodlearena/proto/doodle/game/v1/**")
		exclude("**/*Grpc.java")
	}
	into(layout.buildDirectory.dir("generated/proto"))
}
sourceSets.main {
	java.srcDir(protoSources)
}

dependencies {
	compileOnly("org.apache.flink:flink-streaming-java:$flinkVersion")
	compileOnly("org.apache.flink:flink-table-api-java-bridge:$flinkVersion")
	implementation("org.apache.flink:flink-connector-kafka:$kafkaConnectorVersion")
	implementation("org.apache.flink:flink-connector-base:$flinkVersion")
	implementation("com.google.protobuf:protobuf-java:$protobufVersion")

	testImplementation("org.apache.flink:flink-streaming-java:$flinkVersion")
	testImplementation("org.apache.flink:flink-table-api-java-bridge:$flinkVersion")
	testImplementation("org.apache.flink:flink-test-utils:$flinkVersion")
	// The keyed operator test harness (KeyedOneInputStreamOperatorTestHarness) and what it needs.
	testImplementation("org.apache.flink:flink-streaming-java:$flinkVersion:tests")
	testImplementation("org.apache.flink:flink-runtime:$flinkVersion:tests")
	testImplementation("org.apache.flink:flink-clients:$flinkVersion")
	testRuntimeOnly("org.apache.flink:flink-table-planner-loader:$flinkVersion")
	testRuntimeOnly("org.apache.flink:flink-table-runtime:$flinkVersion")
	testImplementation("org.apache.flink:flink-connector-kafka:$kafkaConnectorVersion")
	testImplementation("org.testcontainers:testcontainers-kafka:2.0.5")
	testImplementation("org.testcontainers:testcontainers-junit-jupiter:2.0.5")
	testImplementation(platform("org.junit:junit-bom:6.1.3"))
	testImplementation("org.junit.jupiter:junit-jupiter")
	testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

tasks.withType<JavaCompile> {
	options.compilerArgs.addAll(listOf("-Xlint:all,-processing,-serial,-try", "-Werror"))
	// Generated code isn't held to -Werror.
	if (name == "compileJava") {
		options.compilerArgs.add("-Xlint:-deprecation")
	}
}

tasks.test {
	useJUnitPlatform()
	// Flink's MiniCluster reflects into the JDK on 17+.
	jvmArgs("--add-opens=java.base/java.util=ALL-UNNAMED", "--add-opens=java.base/java.lang=ALL-UNNAMED")
}

tasks.shadowJar {
	archiveFileName = "analytics.jar"
	mergeServiceFiles()
	manifest {
		attributes["Main-Class"] = "com.doodlearena.analytics.AnalyticsJob"
	}
}
