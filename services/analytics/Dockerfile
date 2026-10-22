# syntax=docker/dockerfile:1
# The analytics service (ADR 0018): the official Flink image with the job jar, run in application
# mode (the jobmanager runs `standalone-job`, the taskmanager `taskmanager`; both need the jar).
# The build context is the repository root, for the generated contracts in proto/gen/java;
# Dockerfile.dockerignore sends only those two folders.
FROM eclipse-temurin:21.0.12.1_1-jdk AS build
WORKDIR /src/services/analytics
COPY proto/gen/java /src/proto/gen/java
COPY services/analytics/gradlew services/analytics/settings.gradle.kts services/analytics/build.gradle.kts ./
COPY services/analytics/gradle ./gradle
RUN --mount=type=cache,target=/root/.gradle ./gradlew --no-daemon --quiet dependencies > /dev/null
COPY services/analytics/src ./src
RUN --mount=type=cache,target=/root/.gradle ./gradlew --no-daemon --quiet shadowJar \
    && cp build/libs/analytics.jar /analytics.jar

FROM flink:2.2.1-java21
# Checkpoints go to S3 (SeaweedFS locally): Flink loads its S3 filesystems as plugins.
RUN mkdir -p /opt/flink/plugins/s3-fs-presto \
    && cp /opt/flink/opt/flink-s3-fs-presto-2.2.1.jar /opt/flink/plugins/s3-fs-presto/
# The OpenTelemetry Java agent, as in the marketplace and the broker (rule: .claude/rules/java.md):
# the Kafka clients' spans, the JVM's metrics and the logs over OTLP. Flink's own metrics go out
# through its OpenTelemetry metric reporter (FLINK_PROPERTIES). Pinned by Maven Central's sha256.
ADD --chmod=0644 \
    --checksum=sha256:f787eb6c7f3d18e69a431e108a15278d25ee37f83d68b678f621e063f3988f82 \
    https://repo1.maven.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent/2.32.0/opentelemetry-javaagent-2.32.0.jar \
    /opt/otel/opentelemetry-javaagent.jar
ENV FLINK_ENV_JAVA_OPTS="-javaagent:/opt/otel/opentelemetry-javaagent.jar"
# JSON logs on stdout, in the services' format (CLAUDE.md, "Telemetry").
COPY services/analytics/conf/log4j-console.properties /opt/flink/conf/log4j-console.properties
COPY --from=build /analytics.jar /opt/flink/usrlib/analytics.jar
