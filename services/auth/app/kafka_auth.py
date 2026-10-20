"""The service's Kafka user (ADR 0013): SCRAM-SHA-512 with its own username and password, over
TLS when a CA is given (SASL_SSL in AWS). The broker's ACLs decide what the user may do. With no
username there is nothing to add, which is what the test brokers expect."""

import os
from collections.abc import Mapping


def kafka_auth(env: Mapping[str, str] = os.environ) -> dict[str, str]:
    """librdkafka settings from KAFKA_USERNAME, KAFKA_PASSWORD and KAFKA_CA_PEM."""
    username = env.get("KAFKA_USERNAME", "")
    password = env.get("KAFKA_PASSWORD", "")
    ca_pem = env.get("KAFKA_CA_PEM", "")
    if not username:
        if ca_pem:
            raise ValueError("KAFKA_CA_PEM is set without KAFKA_USERNAME")
        return {}
    if not password:
        raise ValueError("KAFKA_USERNAME is set without KAFKA_PASSWORD")
    settings = {
        "security.protocol": "SASL_SSL" if ca_pem else "SASL_PLAINTEXT",
        "sasl.mechanism": "SCRAM-SHA-512",
        "sasl.username": username,
        "sasl.password": password,
    }
    if ca_pem:
        settings["ssl.ca.pem"] = ca_pem
    return settings
