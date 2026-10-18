import socket
from typing import Literal

from pydantic import Field
from pydantic_settings import BaseSettings

Mode = Literal["approve", "reject", "review", "fail"]


class Settings(BaseSettings):
    """Configuration read from environment variables (for example SERVICE_VERSION)."""

    port: int = 8080
    # Operator endpoints (/internal/...) answer only on this port, which the gateway never
    # routes and compose never publishes (design doc, section 11).
    internal_port: int = 9091
    service_version: str = "dev"
    service_instance_id: str = socket.gethostname()
    log_level: str = "info"
    # The moderation stub's starting behaviour (Stage 3); PUT /internal/moderation-mode changes
    # it until the next restart.
    moderation_mode: Mode = "approve"
    moderation_delay_ms: int = Field(default=0, ge=0, le=60_000)
    moderation_failure_rate: float = Field(default=0.0, ge=0.0, le=1.0)
