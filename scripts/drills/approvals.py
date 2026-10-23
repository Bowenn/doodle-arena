"""Approves a drawing every few seconds, through the real pipeline, while a load test runs: the
gallery cache's generation moves as it would in play (make loadtest-gallery, ADR 0024).

    uv run --project services/auth --with confluent-kafka --with boto3 \\
        --with 'protobuf>=7.36.2,<8' python scripts/drills/approvals.py <seconds> [interval]

Each round is published as dev-tools, like search.py's drawings; the AI stub approves it.
"""

import sys
import time
import uuid

from search import draw  # the same RoundFinished and stroke log as search-check


def main() -> int:
    seconds = float(sys.argv[1]) if len(sys.argv) > 1 else 120
    interval = float(sys.argv[2]) if len(sys.argv) > 2 else 2
    deadline, n = time.monotonic() + seconds, 0
    while time.monotonic() < deadline:
        draw(f"u_gallery_load_{n % 10}", "loadtest" + uuid.uuid4().hex[:6])
        n += 1
        time.sleep(interval)
    print(f"approvals: {n} rounds published")
    return 0


if __name__ == "__main__":
    sys.exit(main())
