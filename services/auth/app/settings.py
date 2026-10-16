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

    # Access tokens (ADR 0001). The key file is created by scripts/gen-jwt-key.sh.
    jwt_private_key_path: str = ""
    jwt_issuer: str = "http://localhost:8080/api/auth"
    access_token_ttl_seconds: int = 900

    # Refresh tokens and cookies. Secure cookies are off locally (plain http://localhost).
    refresh_token_ttl_days: int = 30
    cookie_secure: bool = False

    # Google sign-in. Without a client id and secret, /google/login returns 503.
    google_client_id: str = ""
    google_client_secret: str = ""
    google_redirect_uri: str = "http://localhost:8080/api/auth/google/callback"
    # Where the web app lives; login redirects back here, and /refresh only accepts this Origin.
    public_base_url: str = "http://localhost:8080"
    oauth_state_secret: str = ""

    @property
    def google_configured(self) -> bool:
        return bool(self.google_client_id and self.google_client_secret)

    def database_url(self) -> str:
        return (
            f"postgresql+psycopg://{self.db_user}:{self.db_password}"
            f"@{self.db_host}:{self.db_port}/{self.db_name}"
        )
