import socket

from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    """Configuration read from environment variables (for example SERVICE_VERSION)."""

    port: int = 8080
    service_version: str = "dev"
    service_instance_id: str = socket.gethostname()
    log_level: str = "info"

    # Postgres database "auth" (owned by this service only).
    db_host: str = "postgres"
    db_port: int = 5432
    db_name: str = "auth"
    db_user: str = "auth"
    db_password: str = ""

    def database_url(self) -> str:
        return (
            f"postgresql+psycopg://{self.db_user}:{self.db_password}"
            f"@{self.db_host}:{self.db_port}/{self.db_name}"
        )
