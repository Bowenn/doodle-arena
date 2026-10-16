import socket

from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    """Configuration read from environment variables (for example SERVICE_VERSION)."""

    port: int = 8080
    service_version: str = "dev"
    service_instance_id: str = socket.gethostname()
    log_level: str = "info"
