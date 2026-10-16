---
paths:
  - "services/marketplace/**"
  - "services/analytics/**"
---

# Java services

- Use the current LTS JDK, Spring Boot for `marketplace` and Apache Flink for `analytics`. Build with the Gradle wrapper.
- Write ledger and ownership SQL explicitly (Spring `JdbcClient` or jOOQ), not through JPA entities, so every lock and transaction boundary is visible and easy to explain.
- Manage the schema with Flyway migrations. Never edit a migration that has been merged; add a new one.
- Test with JUnit 5. Integration tests run against real Postgres and Kafka through Testcontainers, never an in-memory substitute.
- Instrument with the OpenTelemetry Java agent, and add custom metrics through the OpenTelemetry API.
- Flink jobs use event time with watermarks, and are tested with Flink's test harness or a local mini-cluster.
