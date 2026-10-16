# syntax=docker/dockerfile:1
FROM eclipse-temurin:25.0.4.1_1-jdk AS build
WORKDIR /src
COPY gradlew settings.gradle.kts build.gradle.kts ./
COPY gradle ./gradle
RUN --mount=type=cache,target=/root/.gradle ./gradlew --no-daemon --quiet dependencies > /dev/null
COPY src ./src
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
# so the code needs no tracing calls. The jar is pinned by version and Maven Central's sha256.
# JAVA_TOOL_OPTIONS loads it only in the image; `./gradlew test` runs without it. The agent
# reads its settings from the OTEL_* env vars that docker-compose sets. /otel is created above,
# because ADD --chmod would give a directory it creates the file's mode (no x bit).
ADD --chmod=0644 \
    --checksum=sha256:f787eb6c7f3d18e69a431e108a15278d25ee37f83d68b678f621e063f3988f82 \
    https://repo1.maven.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent/2.32.0/opentelemetry-javaagent-2.32.0.jar \
    /otel/opentelemetry-javaagent.jar
ENV JAVA_TOOL_OPTIONS="-javaagent:/otel/opentelemetry-javaagent.jar"
COPY --from=build /app.jar /app/app.jar
USER app
EXPOSE 8080
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75", "-jar", "/app/app.jar"]
