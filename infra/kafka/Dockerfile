# syntax=docker/dockerfile:1
# The broker (ADR 0013): the official image with our start script, its logging config, and the
# OpenTelemetry Java agent, which sends the broker's logs and two JMX metrics over OTLP
# (infra/kafka/jmx.yaml: failed logins and ACL denials, docs/observability.md). The agent is
# the marketplace's, pinned by version and Maven Central's sha256, and loads only into the
# broker's own JVM (broker.sh), never into the CLI tools.
# The agent is downloaded in a stage of its own, retried: a single ADD from Maven Central once
# got a corrupted body in CI (digest mismatch) and failed the build. The digest is still checked.
FROM alpine:3.22.1 AS agent
ARG AGENT_URL=https://repo1.maven.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent/2.32.0/opentelemetry-javaagent-2.32.0.jar
ARG AGENT_SHA256=f787eb6c7f3d18e69a431e108a15278d25ee37f83d68b678f621e063f3988f82
RUN for attempt in 1 2 3 4 5; do \
      wget -q -O /agent.jar "$AGENT_URL" && echo "$AGENT_SHA256  /agent.jar" | sha256sum -c - && exit 0; \
      echo "agent download failed (attempt $attempt), retrying" >&2; rm -f /agent.jar; sleep $((attempt * 5)); \
    done; exit 1

FROM apache/kafka:4.2.2
# 755, not 644: --chmod also sets the directories it creates, which then need the x bit.
COPY --from=agent --chmod=755 /agent.jar /opt/otel/opentelemetry-javaagent.jar
COPY --chmod=755 broker.sh /etc/doodle-arena/kafka-broker.sh
COPY --chmod=755 log4j2.yaml /etc/doodle-arena/kafka-log4j2.yaml
COPY --chmod=755 jmx.yaml /etc/doodle-arena/kafka-jmx.yaml
COPY --chmod=755 admin.sh /usr/local/bin/kafka-admin
ENTRYPOINT ["/bin/bash", "/etc/doodle-arena/kafka-broker.sh"]
