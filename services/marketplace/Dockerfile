# syntax=docker/dockerfile:1
# The build context is the repository root, so the build can include the generated contracts in
# proto/gen/java (settings.gradle.kts includes ../../proto/gen/java). Dockerfile.dockerignore,
# next to this file, sends only those two folders to the builder.
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

FROM eclipse-temurin:25.0.4.1_1-jdk AS build
WORKDIR /src/services/marketplace
COPY proto/gen/java /src/proto/gen/java
COPY services/marketplace/gradlew services/marketplace/settings.gradle.kts services/marketplace/build.gradle.kts ./
COPY services/marketplace/gradle ./gradle
RUN --mount=type=cache,target=/root/.gradle ./gradlew --no-daemon --quiet dependencies > /dev/null
COPY services/marketplace/src ./src
RUN --mount=type=cache,target=/root/.gradle ./gradlew --no-daemon --quiet bootJar \
    && cp build/libs/marketplace-*.jar /app.jar

FROM eclipse-temurin:25.0.4.1_1-jre
# curl is only for the container healthcheck; the JRE image has neither curl nor wget.
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 app \
    && mkdir /otel
# Traces, metrics and logs come from the OpenTelemetry Java agent (rule: .claude/rules/java.md),
# so the code needs no tracing calls. The jar is pinned by version and Maven Central's sha256
# (the agent stage above).
# JAVA_TOOL_OPTIONS loads it only in the image; `./gradlew test` runs without it. The agent
# reads its settings from the OTEL_* env vars that docker-compose sets. /otel is created above,
# because COPY --chmod would give a directory it creates the file's mode (no x bit).
COPY --from=agent --chmod=0644 /agent.jar /otel/opentelemetry-javaagent.jar
ENV JAVA_TOOL_OPTIONS="-javaagent:/otel/opentelemetry-javaagent.jar"
COPY --from=build /app.jar /app/app.jar
USER app
EXPOSE 8080
# Serial GC with a fixed heap of half the container's memory (512 MB of the 1 GB in compose and
# on ECS). Serial, because the ECS task has 0.5 vCPU, which the JVM rounds to one processor: a
# concurrent collector's threads would share that core with the requests. Fixed, because the
# JVM otherwise starts at 1/64 of memory and grows the heap only through full collections: under
# load a small young generation was collected every 0.6 s and a 58 MB old generation filled up.
# Half, because the rest of the process (metaspace, code cache, threads, the agent) takes about
# 350 MB, so 75% would leave no room below the limit.
# SweeperThreshold: once the JIT has added 15% of the 240 MB code cache (the default), the JVM
# collects to unload old code, and under Serial that is a full collection with class unloading:
# 1.4 s, once per JVM, a minute or two into the first load. At 50% warm-up never reaches it (the
# code cache settles near 45 MB); a nearly full code cache still unloads, at 10% free.
ENTRYPOINT ["java", "-XX:+UseSerialGC", "-XX:InitialRAMPercentage=50", "-XX:MaxRAMPercentage=50", "-XX:SweeperThreshold=50", "-jar", "/app/app.jar"]
