"""The auth service's domain metrics (docs/observability.md, section 3).

Attributes are small, fixed sets of values. IDs never go into metric attributes.
"""

from typing import Literal

from opentelemetry import metrics
from opentelemetry.metrics import Meter

from app import SERVICE_NAME

LoginResult = Literal["success", "failed"]
RefreshResult = Literal["rotated", "reuse_detected", "expired", "invalid"]
ApiKeyResult = Literal["valid", "invalid", "revoked"]


class AuthMetrics:
    def __init__(self, meter: Meter | None = None) -> None:
        # The global meter is a no-op until telemetry.setup_telemetry installs a provider.
        meter = meter or metrics.get_meter(SERVICE_NAME)
        self._logins = meter.create_counter(
            "auth.logins", unit="{login}", description="Completed Google sign-ins, by result"
        )
        self._refreshes = meter.create_counter(
            "auth.refreshes", unit="{refresh}", description="Refresh-token uses, by result"
        )

        self._api_key_verifications = meter.create_counter(
            "auth.api_keys.verifications",
            unit="{verification}",
            description="API key checks from other services, by result",
        )

    def login(self, result: LoginResult) -> None:
        self._logins.add(1, {"result": result})

    def refresh(self, result: RefreshResult) -> None:
        self._refreshes.add(1, {"result": result})

    def api_key_verification(self, result: ApiKeyResult) -> None:
        self._api_key_verifications.add(1, {"result": result})
