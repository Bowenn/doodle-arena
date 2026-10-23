"""`python manage.py reindex_search`: rebuild the search index from Postgres (ADR 0023).

Safe while the indexer runs: a new index is filled, the alias drawings moves to it in one step,
and what changed during the build is written again; external versions refuse stale writes. Run
it in the assets-indexer container, whose Elasticsearch user may create indices and move the
alias.
"""

import json
from typing import Any

from django.core.management.base import BaseCommand

from drawings import search


class Command(BaseCommand):
    help = "Rebuild the search index from Postgres and swap the alias drawings to it."

    def handle(self, *args: Any, **options: Any) -> None:
        self.stdout.write(json.dumps(search.reindex()))
