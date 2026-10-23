"""`python manage.py search_bench`: search on the seeded data (ADR 0023, docs/performance.md).

Rebuilds the index from Postgres (every approved drawing), then runs typical searches, 30 times
each after one warm-up, and prints the median and p95 of Elasticsearch's own time and of the
whole search with its hydration from Postgres. Run it in the assets-indexer container.
"""

import json
import statistics
import time
from typing import Any

from django.core.management.base import BaseCommand

from drawings import search
from drawings.models import Drawing, SearchListing, Status

RUNS = 30


class Command(BaseCommand):
    help = "Rebuild the search index and time typical searches."

    def handle(self, *args: Any, **options: Any) -> None:
        es = search.client()
        rebuilt = search.reindex(es)
        common = (
            Drawing.objects.filter(status=Status.APPROVED).values_list("word", flat=True).first()
            or "cat"
        )
        queries = {
            "word": search.Query(q=common),
            "prefix": search.Query(q=common[:3]),
            "typo": search.Query(
                q=common[:-2] + common[-1] + common[-2] if len(common) > 3 else common
            ),
            "listed, by price": search.Query(listed=True, sort="price_asc"),
            "word, listed, price range": search.Query(
                q=common, listed=True, min_price=10, max_price=500
            ),
            "newest, page 1": search.Query(sort="newest"),
        }
        results: dict[str, Any] = {"rebuild": rebuilt}
        for name, query in queries.items():
            search.search(query, es)  # warm-up
            took, total = [], []
            for _ in range(RUNS):
                started = time.monotonic()
                hits = search.search(query, es)
                es_ms = (time.monotonic() - started) * 1000
                list(Drawing.objects.filter(id__in=hits.ids, status=Status.APPROVED))
                list(SearchListing.objects.filter(drawing_id__in=hits.ids))
                took.append(es_ms)
                total.append((time.monotonic() - started) * 1000)
            results[name] = {
                "hits": hits.total,
                "es_p50_ms": round(statistics.median(took), 1),
                "es_p95_ms": round(sorted(took)[int(RUNS * 0.95) - 1], 1),
                "with_hydration_p50_ms": round(statistics.median(total), 1),
                "with_hydration_p95_ms": round(sorted(total)[int(RUNS * 0.95) - 1], 1),
            }
        self.stdout.write(json.dumps(results, indent=2))
