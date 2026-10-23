"""Search over approved drawings and their active listings (ADR 0023).

Elasticsearch holds one document per approved drawing, behind the alias "drawings". Every
document is built whole from assets' own Postgres (the drawing row and the search projection of
its listing) and written with the drawing's search_version as an external version, so a retry, a
reorder or the rebuild can never put an older document over a newer one.

Elasticsearch ranks; Postgres answers: a search returns ids, and the API hydrates them from
Postgres, which re-checks that each drawing is approved.
"""

import base64
import binascii
import json
import logging
import time
import uuid
from dataclasses import dataclass, field
from functools import cache
from typing import Any

from django.conf import settings
from elasticsearch import ConflictError, Elasticsearch, NotFoundError
from elasticsearch.helpers import streaming_bulk

from drawings.models import Drawing, SearchListing, Status

ALIAS = "drawings"

log = logging.getLogger(__name__)

# Two analyzers over the same word: whole words (with fuzziness at query time, for typos) and
# their edge n-grams (for prefixes as you type). asciifolding makes "café" match "cafe".
INDEX_SETTINGS: dict[str, Any] = {
    "number_of_shards": 1,
    "number_of_replicas": 0,
    "analysis": {
        "filter": {"prefix_ngram": {"type": "edge_ngram", "min_gram": 2, "max_gram": 20}},
        "analyzer": {
            "word_text": {
                "type": "custom",
                "tokenizer": "standard",
                "filter": ["lowercase", "asciifolding"],
            },
            "word_prefix": {
                "type": "custom",
                "tokenizer": "standard",
                "filter": ["lowercase", "asciifolding", "prefix_ngram"],
            },
        },
    },
}

MAPPINGS: dict[str, Any] = {
    # A field not listed here is refused: the document's shape is a contract.
    "dynamic": "strict",
    "properties": {
        "id": {"type": "keyword"},
        "word": {
            "type": "text",
            "analyzer": "word_text",
            "fields": {
                "prefix": {
                    "type": "text",
                    "analyzer": "word_prefix",
                    "search_analyzer": "word_text",
                },
            },
        },
        "author_ids": {"type": "keyword"},
        "match_id": {"type": "keyword"},
        "ended_at": {"type": "date"},
        "listed": {"type": "boolean"},
        "listing": {
            "properties": {
                "listing_id": {"type": "keyword"},
                "seller_id": {"type": "keyword"},
                "price": {"type": "long"},
                "listed_at": {"type": "date"},
            }
        },
    },
}

# The price facet's buckets, in coins (listings cost 1 to 1,000,000).
PRICE_RANGES: list[dict[str, Any]] = [
    {"key": "1-10", "from": 1, "to": 11},
    {"key": "11-50", "from": 11, "to": 51},
    {"key": "51-200", "from": 51, "to": 201},
    {"key": "201-1000", "from": 201, "to": 1001},
    {"key": "1000+", "from": 1001},
]


class SearchUnavailable(Exception):
    """Elasticsearch isn't configured, or can't be reached."""


@cache
def client() -> Elasticsearch:
    """The shared client. Every call has a 2-second timeout (searches are on the request path);
    nothing retries inside it. Index management uses admin()."""
    if not settings.ELASTICSEARCH_URL:
        raise SearchUnavailable("ELASTICSEARCH_URL is not set")
    auth = None
    if settings.ELASTICSEARCH_USERNAME:
        auth = (settings.ELASTICSEARCH_USERNAME, settings.ELASTICSEARCH_PASSWORD)
    return Elasticsearch(
        settings.ELASTICSEARCH_URL,
        basic_auth=auth,
        request_timeout=2,
        max_retries=0,
        retry_on_timeout=False,
    )


# Creating an index, a bulk chunk or an alias swap can take seconds on a busy node.
ADMIN_TIMEOUT = 30


def admin(es: Elasticsearch) -> Elasticsearch:
    """The same client with the longer timeout of index management."""
    return es.options(request_timeout=ADMIN_TIMEOUT)


def new_index_name() -> str:
    return f"{ALIAS}-v{time.time_ns() // 1_000_000}"


def ensure_index(es: Elasticsearch) -> None:
    """Creates the first index and points the alias at it, if there is no alias yet."""
    es = admin(es)
    if es.indices.exists_alias(name=ALIAS):
        return
    name = new_index_name()
    es.indices.create(index=name, settings=INDEX_SETTINGS, mappings=MAPPINGS, aliases={ALIAS: {}})
    log.info("search index created", extra={"index": name})


def document(drawing: Drawing, listing: SearchListing | None) -> dict[str, Any]:
    """The search document of an approved drawing, from Postgres alone."""
    return {
        "id": str(drawing.id),
        "word": drawing.word,
        "author_ids": list(drawing.author_ids),
        "match_id": drawing.match_id,
        "ended_at": drawing.ended_at.isoformat(),
        "listed": listing is not None,
        "listing": None
        if listing is None
        else {
            "listing_id": str(listing.listing_id),
            "seller_id": listing.seller_id,
            "price": listing.price,
            "listed_at": listing.listed_at.isoformat(),
        },
    }


def index_drawing(
    drawing_id: uuid.UUID, es: Elasticsearch | None = None, index: str = ALIAS
) -> str:
    """Writes the drawing's current document (or deletes it if it isn't approved), with its
    search_version. Returns indexed, deleted, absent (no such drawing) or stale (Elasticsearch
    already has a newer version: someone wrote after us)."""
    es = es or client()
    drawing = Drawing.objects.filter(id=drawing_id).first()
    if drawing is None:
        return "absent"
    try:
        if drawing.status != Status.APPROVED:
            es.delete(
                index=index,
                id=str(drawing.id),
                version=drawing.search_version,
                version_type="external",
            )
            return "deleted"
        listing = SearchListing.objects.filter(drawing_id=drawing.id).first()
        es.index(
            index=index,
            id=str(drawing.id),
            document=document(drawing, listing),
            version=drawing.search_version,
            version_type="external",
        )
        return "indexed"
    except ConflictError:
        return "stale"
    except NotFoundError:
        return "deleted"


def reindex(es: Elasticsearch | None = None, batch: int = 2000) -> dict[str, Any]:
    """Rebuilds the index from Postgres while the indexer keeps running: a new index, every
    approved drawing in bulk, the alias swapped in one step, then a catch-up pass for what changed
    during the build (the indexer wrote those to the old index). External versions make every
    write safe to repeat."""
    es = admin(es or client())
    started = time.monotonic()
    name = new_index_name()
    es.indices.create(index=name, settings=INDEX_SETTINGS, mappings=MAPPINGS)
    old = (
        list(es.indices.get_alias(name=ALIAS).keys()) if es.indices.exists_alias(name=ALIAS) else []
    )

    snapshot: dict[uuid.UUID, int] = {}
    listings = {
        row.drawing_id: row for row in SearchListing.objects.all().iterator(chunk_size=batch)
    }

    def actions() -> Any:
        query = Drawing.objects.filter(status=Status.APPROVED).order_by("id")
        for drawing in query.iterator(chunk_size=batch):
            snapshot[drawing.id] = drawing.search_version
            yield {
                "_op_type": "index",
                "_index": name,
                "_id": str(drawing.id),
                "version": drawing.search_version,
                "version_type": "external",
                "_source": document(drawing, listings.get(drawing.id)),
            }

    indexed = 0
    for ok, item in streaming_bulk(es, actions(), chunk_size=batch, raise_on_error=False):
        if ok:
            indexed += 1
        elif item.get("index", {}).get("status") != 409:
            raise RuntimeError(f"bulk index failed: {item}")
    es.indices.refresh(index=name)
    es.indices.update_aliases(
        actions=[{"add": {"index": name, "alias": ALIAS}}]
        + [{"remove": {"index": o, "alias": ALIAS}} for o in old]
    )
    # The catch-up: drawings whose version moved, or that were approved, while the bulk ran.
    changed = [
        d.id
        for d in Drawing.objects.only("id", "search_version", "status").iterator(chunk_size=batch)
        if (d.status == Status.APPROVED and snapshot.get(d.id) != d.search_version)
        or (d.status != Status.APPROVED and d.id in snapshot)
    ]
    for drawing_id in changed:
        index_drawing(drawing_id, es)
    for o in old:
        es.indices.delete(index=o)
    return {
        "index": name,
        "indexed": indexed,
        "caught_up": len(changed),
        "replaced": old,
        "seconds": round(time.monotonic() - started, 1),
    }


SORTS: dict[str, list[dict[str, Any]]] = {
    "relevance": [{"_score": "desc"}, {"ended_at": "desc"}, {"id": "desc"}],
    "newest": [{"ended_at": "desc"}, {"id": "desc"}],
    "price_asc": [{"listing.price": {"order": "asc", "missing": "_last"}}, {"id": "asc"}],
    "price_desc": [{"listing.price": {"order": "desc", "missing": "_last"}}, {"id": "desc"}],
}


@dataclass(frozen=True)
class Query:
    q: str = ""
    listed: bool = False
    min_price: int | None = None
    max_price: int | None = None
    author_id: str = ""
    sort: str = "relevance"
    limit: int = 20
    after: list[Any] | None = field(default=None)


def body(query: Query) -> dict[str, Any]:
    """The search request: the words scored, everything else as filters (cached, not scored)."""
    filters: list[dict[str, Any]] = []
    if query.listed or query.min_price is not None or query.max_price is not None:
        filters.append({"term": {"listed": True}})
    if query.min_price is not None or query.max_price is not None:
        price: dict[str, int] = {}
        if query.min_price is not None:
            price["gte"] = query.min_price
        if query.max_price is not None:
            price["lte"] = query.max_price
        filters.append({"range": {"listing.price": price}})
    if query.author_id:
        filters.append({"term": {"author_ids": query.author_id}})
    must: dict[str, Any]
    if query.q:
        must = {
            "bool": {
                "should": [
                    # Whole words, forgiving a typo or two ("elephnt" finds elephant); exact
                    # matches score higher.
                    {"match": {"word": {"query": query.q, "fuzziness": "AUTO", "boost": 2}}},
                    # Prefixes as you type ("eleph" finds elephant).
                    {"match": {"word.prefix": {"query": query.q}}},
                ],
                "minimum_should_match": 1,
            }
        }
    else:
        must = {"match_all": {}}
    request: dict[str, Any] = {
        "query": {"bool": {"must": [must], "filter": filters}},
        "sort": SORTS[query.sort if query.q or query.sort != "relevance" else "newest"],
        "size": query.limit,
        "track_total_hits": True,
        "aggs": {"price": {"range": {"field": "listing.price", "ranges": PRICE_RANGES}}},
        "_source": False,
    }
    if query.after is not None:
        request["search_after"] = query.after
    return request


@dataclass(frozen=True)
class Hits:
    ids: list[uuid.UUID]
    total: int
    facets: list[dict[str, Any]]
    next_after: list[Any] | None


def search(query: Query, es: Elasticsearch | None = None) -> Hits:
    es = es or client()
    response = es.search(index=ALIAS, **body(query))
    hits = response["hits"]["hits"]
    buckets = response["aggregations"]["price"]["buckets"]
    facets = [
        {
            "key": b["key"],
            "from": b.get("from"),
            "to": (b["to"] - 1) if "to" in b else None,
            "count": b["doc_count"],
        }
        for b in buckets
    ]
    next_after = hits[-1]["sort"] if len(hits) == query.limit else None
    return Hits(
        ids=[uuid.UUID(h["_id"]) for h in hits],
        total=response["hits"]["total"]["value"],
        facets=facets,
        next_after=next_after,
    )


def encode_cursor(after: list[Any]) -> str:
    return (
        base64.urlsafe_b64encode(json.dumps(after, separators=(",", ":")).encode())
        .decode()
        .rstrip("=")
    )


def decode_cursor(cursor: str) -> list[Any]:
    """A cursor from encode_cursor; ValueError if it isn't one."""
    try:
        padded = cursor + "=" * (-len(cursor) % 4)
        value = json.loads(base64.urlsafe_b64decode(padded.encode()))
    except (binascii.Error, ValueError, UnicodeDecodeError) as e:
        raise ValueError("invalid cursor") from e
    if not isinstance(value, list) or not all(
        isinstance(v, (str, int, float)) or v is None for v in value
    ):
        raise ValueError("invalid cursor")
    return value
