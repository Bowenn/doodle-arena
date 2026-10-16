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
    && useradd --system --uid 10001 app
COPY --from=build /app.jar /app/app.jar
USER app
EXPOSE 8080
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75", "-jar", "/app/app.jar"]
