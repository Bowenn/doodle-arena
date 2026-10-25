# syntax=docker/dockerfile:1
# The analytics service (ADR 0018): the official Flink image with the job jar, run in application
# mode (the jobmanager runs `standalone-job`, the taskmanager `taskmanager`; both need the jar).
# The build context is the repository root, for the generated contracts in proto/gen/java;
# Dockerfile.dockerignore sends only those two folders.
# The OpenTelemetry Java agent, downloaded in a stage of its own and retried, as in
# infra/kafka/Dockerfile: a single ADD from Maven Central got a corrupted body in CI twice (a
# digest mismatch). The sha256 is still checked.
FROM alpine:3.22.1 AS agent
ARG AGENT_URL=https://repo1.maven.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent/2.32.0/opentelemetry-javaagent-2.32.0.jar
ARG AGENT_SHA256=f787eb6c7f3d18e69a431e108a15278d25ee37f83d68b678f621e063f3988f82
RUN for attempt in 1 2 3 4 5; do \
      wget -q -O /agent.jar "$AGENT_URL" && echo "$AGENT_SHA256  /agent.jar" | sha256sum -c - && exit 0; \
      echo "agent download failed (attempt $attempt), retrying" >&2; rm -f /agent.jar; sleep $((attempt * 5)); \
    done; exit 1

FROM eclipse-temurin:21.0.12.1_1-jdk AS build
WORKDIR /src/services/analytics
COPY proto/gen/java /src/proto/gen/java
COPY services/analytics/gradlew services/analytics/settings.gradle.kts services/analytics/build.gradle.kts ./
COPY services/analytics/gradle ./gradle
# A Gradle cache of its own: the marketplace's build mounts /root/.gradle too, and two Gradle
# processes in parallel builds would wait on each other's lock in one shared cache until one
# times out (a fresh CI runner builds both at once).
RUN --mount=type=cache,id=gradle-analytics,target=/root/.gradle ./gradlew --no-daemon --quiet dependencies > /dev/null
COPY services/analytics/src ./src
RUN --mount=type=cache,id=gradle-analytics,target=/root/.gradle ./gradlew --no-daemon --quiet shadowJar \
    && cp build/libs/analytics.jar /analytics.jar

FROM flink:2.2.1-java21
# Checkpoints go to S3 (SeaweedFS locally): Flink loads its S3 filesystems as plugins.
# Two of them, chosen by scheme: s3p:// (Presto) with SeaweedFS's keys locally, s3a:// (Hadoop) in
# AWS, whose default credential chain includes the ECS task role's container credentials
# (Presto's falls back to EC2's instance profile, which Fargate tasks don't have). Task 6.9.
RUN mkdir -p /opt/flink/plugins/s3-fs-presto /opt/flink/plugins/s3-fs-hadoop \
    && cp /opt/flink/opt/flink-s3-fs-presto-2.2.1.jar /opt/flink/plugins/s3-fs-presto/ \
    && cp /opt/flink/opt/flink-s3-fs-hadoop-2.2.1.jar /opt/flink/plugins/s3-fs-hadoop/
# The OpenTelemetry Java agent, as in the marketplace and the broker (rule: .claude/rules/java.md):
# the Kafka clients' spans, the JVM's metrics and the logs over OTLP. Flink's own metrics go out
# through its OpenTelemetry metric reporter (FLINK_PROPERTIES). Pinned by Maven Central's sha256
# (the agent stage above). The image builds as the flink user; the directory is made as root
# first, because COPY --chmod would give a directory it creates the file's 0644, which the flink
# user can't enter.
USER root
RUN install -d -m 0755 /opt/otel
USER flink
COPY --from=agent --chmod=0644 /agent.jar /opt/otel/opentelemetry-javaagent.jar
# The role options (env.java.opts.jobmanager and .taskmanager), not FLINK_ENV_JAVA_OPTS: that one
# replaces env.java.opts.all, whose --add-opens flags Flink needs on Java 21.
ENV FLINK_ENV_JAVA_OPTS_JM="-javaagent:/opt/otel/opentelemetry-javaagent.jar" \
    FLINK_ENV_JAVA_OPTS_TM="-javaagent:/opt/otel/opentelemetry-javaagent.jar"
# JSON logs on stdout, in the services' format (CLAUDE.md, "Telemetry").
COPY services/analytics/conf/log4j-console.properties /opt/flink/conf/log4j-console.properties
COPY --from=build /analytics.jar /opt/flink/usrlib/analytics.jar
