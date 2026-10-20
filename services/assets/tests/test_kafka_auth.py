import pytest

from drawings.kafka_auth import kafka_auth


def test_no_username_adds_nothing() -> None:
    assert kafka_auth({}) == {}


def test_username_adds_scram() -> None:
    assert kafka_auth({"KAFKA_USERNAME": "assets", "KAFKA_PASSWORD": "secret"}) == {
        "security.protocol": "SASL_PLAINTEXT",
        "sasl.mechanism": "SCRAM-SHA-512",
        "sasl.username": "assets",
        "sasl.password": "secret",
    }


def test_ca_adds_tls() -> None:
    settings = kafka_auth(
        {"KAFKA_USERNAME": "assets", "KAFKA_PASSWORD": "s", "KAFKA_CA_PEM": "pem"}
    )
    assert settings["security.protocol"] == "SASL_SSL"
    assert settings["ssl.ca.pem"] == "pem"


@pytest.mark.parametrize(
    "env", [{"KAFKA_USERNAME": "assets"}, {"KAFKA_CA_PEM": "pem"}], ids=["no password", "CA only"]
)
def test_incomplete_config_is_an_error(env: dict[str, str]) -> None:
    with pytest.raises(ValueError):
        kafka_auth(env)
