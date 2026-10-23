"""Search (ADR 0023): the projection's rules in every order, the documents in a real
Elasticsearch with external versions, the search API, and the rebuild."""

import itertools
import threading
import uuid
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest
from django.test import Client
from doodle.events.v1 import drawings_pb2, listings_pb2, sales_pb2

from drawings import indexer, moderation, search
from drawings.models import DecidedBy, Drawing, SearchClosedListing, SearchListing
from tests.conftest import IMAGE

pytestmark = pytest.mark.django_db
API = "/api/assets/search"
T0 = datetime(2026, 10, 20, 12, 0, tzinfo=UTC)


@pytest.fixture
def as_user(signer: Any) -> Any:
    def headers(user_id: str = "u_viewer") -> dict[str, str]:
        return {"Authorization": f"Bearer {signer.token(user_id, ['player'])}"}

    return headers


def approved(make_drawing: Any, word: str, **fields: Any) -> Drawing:
    d: Drawing = make_drawing(
        **{**IMAGE, "word": word, "match_id": f"m_{uuid.uuid4().hex[:8]}", **fields}
    )
    moderation.decide(d.id, approve=True, by=DecidedBy.AI)
    d.refresh_from_db()
    return d


def created(d: uuid.UUID, listing: uuid.UUID, price: int, at: datetime) -> tuple[str, bytes]:
    ev = listings_pb2.ListingEvent()
    c = ev.listing_created
    c.meta.event_id = str(uuid.uuid4())
    c.meta.occurred_at.FromDatetime(at)
    c.listing_id, c.drawing_id, c.seller_id, c.price = str(listing), str(d), "u_seller", price
    c.created_at.FromDatetime(at)
    return indexer.LISTINGS_TOPIC, ev.SerializeToString()


def cancelled(d: uuid.UUID, listing: uuid.UUID, at: datetime) -> tuple[str, bytes]:
    ev = listings_pb2.ListingEvent()
    x = ev.listing_cancelled
    x.meta.occurred_at.FromDatetime(at)
    x.listing_id, x.drawing_id, x.seller_id = str(listing), str(d), "u_seller"
    x.cancelled_at.FromDatetime(at)
    return indexer.LISTINGS_TOPIC, ev.SerializeToString()


def sold(d: uuid.UUID, listing: uuid.UUID, at: datetime) -> tuple[str, bytes]:
    ev = sales_pb2.SaleEvent()
    s = ev.sale_completed
    s.meta.occurred_at.FromDatetime(at)
    s.sale_id, s.listing_id, s.drawing_id = str(uuid.uuid4()), str(listing), str(d)
    s.seller_id, s.buyer_id, s.price = "u_seller", "u_buyer", 10
    s.completed_at.FromDatetime(at)
    return indexer.SALES_TOPIC, ev.SerializeToString()


def approved_event(d: uuid.UUID) -> tuple[str, bytes]:
    ev = drawings_pb2.DrawingEvent()
    a = ev.drawing_approved
    a.meta.occurred_at.FromDatetime(T0)
    a.drawing_id = str(d)
    return indexer.DRAWINGS_TOPIC, ev.SerializeToString()


def active(d: uuid.UUID) -> uuid.UUID | None:
    row = SearchListing.objects.filter(drawing_id=d).first()
    return None if row is None else row.listing_id


# ---------------------------------------------------------------------------------- projection


@pytest.mark.parametrize(
    "story",
    [
        # L1 sold, then L2 listed: L2 is active, whatever the order.
        "sold_then_relisted",
        # L1 cancelled, L2 listed, L2 sold: nothing is active.
        "cancelled_relisted_sold",
    ],
)
def test_every_order_of_a_drawings_events_ends_the_same(make_drawing: Any, story: str) -> None:
    d = approved(make_drawing, "kite").id
    l1, l2 = uuid.uuid4(), uuid.uuid4()
    if story == "sold_then_relisted":
        events = [
            created(d, l1, 10, T0),
            sold(d, l1, T0 + timedelta(minutes=1)),
            created(d, l2, 20, T0 + timedelta(minutes=2)),
        ]
        want = l2
    else:
        events = [
            created(d, l1, 10, T0),
            cancelled(d, l1, T0 + timedelta(minutes=1)),
            created(d, l2, 20, T0 + timedelta(minutes=2)),
            sold(d, l2, T0 + timedelta(minutes=3)),
        ]
        want = None
    for order in itertools.permutations(events):
        SearchListing.objects.filter(drawing_id=d).delete()
        SearchClosedListing.objects.filter(drawing_id=d).delete()
        for topic, value in order:
            indexer.apply(topic, value)
        # Delivered twice: nothing changes.
        for topic, value in order:
            indexer.apply(topic, value)
        assert active(d) == want, order


def test_the_version_only_grows_and_redelivery_changes_nothing(make_drawing: Any) -> None:
    d = approved(make_drawing, "kite")
    l1 = uuid.uuid4()
    start = Drawing.objects.get(id=d.id).search_version
    indexer.apply(*created(d.id, l1, 10, T0))
    after_create = Drawing.objects.get(id=d.id).search_version
    indexer.apply(*created(d.id, l1, 10, T0))
    assert Drawing.objects.get(id=d.id).search_version == after_create > start
    indexer.apply(*sold(d.id, l1, T0 + timedelta(minutes=1)))
    assert Drawing.objects.get(id=d.id).search_version > after_create


def test_unreadable_records_are_skipped(make_drawing: Any) -> None:
    for topic, value in [
        (indexer.LISTINGS_TOPIC, b"\x0a\xff\x01"),
        (indexer.SALES_TOPIC, sales_pb2.SaleEvent().SerializeToString()),
        created(uuid.uuid4(), uuid.uuid4(), 0, T0),  # no price
    ]:
        with pytest.raises(indexer.Skip):
            indexer.apply(topic, value)
    ev = listings_pb2.ListingEvent()
    ev.listing_created.drawing_id = "not-a-uuid"
    with pytest.raises(indexer.Skip):
        indexer.apply(indexer.LISTINGS_TOPIC, ev.SerializeToString())


# ---------------------------------------------------------------------------------- documents


def test_a_drawings_document_follows_its_listing(make_drawing: Any, es: Any) -> None:
    search.ensure_index(es)
    d = approved(make_drawing, "lighthouse")
    l1 = uuid.uuid4()
    indexer.apply(*approved_event(d.id))
    assert search.index_drawing(d.id, es) == "indexed"
    indexer.apply(*created(d.id, l1, 42, T0))
    search.index_drawing(d.id, es)
    doc = es.get(index=search.ALIAS, id=str(d.id))
    assert doc["_source"]["listed"] is True
    assert doc["_source"]["listing"]["price"] == 42
    assert doc["_version"] == Drawing.objects.get(id=d.id).search_version

    indexer.apply(*sold(d.id, l1, T0 + timedelta(minutes=1)))
    search.index_drawing(d.id, es)
    assert es.get(index=search.ALIAS, id=str(d.id))["_source"]["listed"] is False


def test_an_older_version_never_replaces_a_newer_one(make_drawing: Any, es: Any) -> None:
    search.ensure_index(es)
    d = approved(make_drawing, "lighthouse")
    indexer.apply(*created(d.id, uuid.uuid4(), 42, T0))
    assert search.index_drawing(d.id, es) == "indexed"
    newest = es.get(index=search.ALIAS, id=str(d.id))["_version"]
    # A stale writer (a retry, the rebuild) with the version before.
    Drawing.objects.filter(id=d.id).update(search_version=newest - 1)
    assert search.index_drawing(d.id, es) == "stale"
    assert es.get(index=search.ALIAS, id=str(d.id))["_version"] == newest


def test_a_drawing_that_isnt_approved_has_no_document(make_drawing: Any, es: Any) -> None:
    search.ensure_index(es)
    d = make_drawing(**IMAGE, word="ghost")
    moderation.decide(d.id, approve=False, by=DecidedBy.AI)
    indexer.apply(*approved_event(d.id))  # any event for it: the row decides
    assert search.index_drawing(d.id, es) == "deleted"
    assert es.options(ignore_status=404).get(index=search.ALIAS, id=str(d.id))["found"] is False


def test_elasticsearch_down_leaves_the_record_for_a_retry(make_drawing: Any, settings: Any) -> None:
    d = approved(make_drawing, "kite")
    settings.ELASTICSEARCH_URL = "http://127.0.0.1:1"
    search.client.cache_clear()
    try:
        topic, value = created(d.id, uuid.uuid4(), 5, T0)

        class Record:
            def topic(self) -> str:
                return topic

            def value(self) -> bytes:
                return value

            def headers(self) -> None:
                return None

            def partition(self) -> int:
                return 0

            def offset(self) -> int:
                return 7

        stop = threading.Event()
        sleeps: list[float] = []

        def sleep(s: float) -> None:
            sleeps.append(s)
            if len(sleeps) == 2:
                stop.set()

        assert indexer.handle_with_retry(Record(), search.client(), stop, sleep=sleep) is False  # type: ignore[arg-type]
        assert sleeps == [0.5, 1.0]
    finally:
        search.client.cache_clear()


# ---------------------------------------------------------------------------------- the API


@pytest.fixture
def catalog(make_drawing: Any, es: Any) -> dict[str, Drawing]:
    search.ensure_index(es)
    drawings = {
        "elephant": approved(make_drawing, "elephant", author_ids=["u_ada"]),
        "elegant": approved(make_drawing, "elegant", author_ids=["u_bob"]),
        "cat": approved(make_drawing, "cat", author_ids=["u_ada"]),
        "catapult": approved(make_drawing, "catapult", author_ids=["u_cyd"]),
    }
    prices = {"elephant": 30, "cat": 5, "catapult": 300}
    for word, price in prices.items():
        indexer.apply(*created(drawings[word].id, uuid.uuid4(), price, T0))
    for d in drawings.values():
        search.index_drawing(d.id, es)
    es.indices.refresh(index=search.ALIAS)
    return drawings


def words(resp: Any) -> list[str]:
    return [d["word"] for d in resp.json()["drawings"]]


def test_words_with_a_typo_or_a_prefix_find_the_drawing(
    client: Client, as_user: Any, catalog: dict[str, Drawing]
) -> None:
    # "elephnt" is one edit from elephant and two from elegant. ("elefant" would rightly find
    # elegant first: one substitution, against two edits for elephant.)
    assert words(client.get(f"{API}?q=elephnt", headers=as_user()))[0] == "elephant"
    assert set(words(client.get(f"{API}?q=ele", headers=as_user()))) == {"elephant", "elegant"}
    # The exact word ranks above a word it is a prefix of.
    assert words(client.get(f"{API}?q=cat", headers=as_user()))[:2] == ["cat", "catapult"]
    assert words(client.get(f"{API}?q=zebra", headers=as_user())) == []


def test_filters_sorts_and_the_price_facet(
    client: Client, as_user: Any, catalog: dict[str, Drawing]
) -> None:
    listed = client.get(f"{API}?listed=true&sort=price_asc", headers=as_user()).json()
    assert [d["word"] for d in listed["drawings"]] == ["cat", "elephant", "catapult"]
    assert [d["listing"]["price"] for d in listed["drawings"]] == [5, 30, 300]
    assert {f["key"]: f["count"] for f in listed["facets"]["price"]} == {
        "1-10": 1,
        "11-50": 1,
        "51-200": 0,
        "201-1000": 1,
        "1000+": 0,
    }
    ranged = client.get(f"{API}?min_price=10&max_price=100", headers=as_user())
    assert words(ranged) == ["elephant"]
    assert set(words(client.get(f"{API}?author_id=u_ada", headers=as_user()))) == {
        "elephant",
        "cat",
    }
    unlisted = client.get(f"{API}?q=elegant", headers=as_user()).json()["drawings"][0]
    assert unlisted["listing"] is None and unlisted["thumb_url"]


def test_the_cursor_pages_without_gaps_or_repeats(
    client: Client, as_user: Any, catalog: dict[str, Drawing]
) -> None:
    seen: list[str] = []
    url = f"{API}?sort=newest&limit=1"
    for _ in range(10):
        page = client.get(url, headers=as_user()).json()
        seen += [d["word"] for d in page["drawings"]]
        if not page["next_cursor"]:
            break
        url = f"{API}?sort=newest&limit=1&cursor={page['next_cursor']}"
    assert sorted(seen) == sorted(catalog)


def test_postgres_has_the_last_word(
    client: Client, as_user: Any, catalog: dict[str, Drawing]
) -> None:
    # Rejected after it was indexed (the index hasn't heard yet): the API leaves it out.
    Drawing.objects.filter(id=catalog["cat"].id).update(status="rejected")
    assert "cat" not in words(client.get(f"{API}?q=cat", headers=as_user()))


def test_bad_parameters_and_no_elasticsearch(client: Client, as_user: Any, settings: Any) -> None:
    for query in [
        "sort=cheapest",
        "limit=0",
        "limit=51",
        "min_price=-1",
        "listed=maybe",
        "cursor=%%%",
        "q=" + "x" * 65,
    ]:
        assert client.get(f"{API}?{query}", headers=as_user()).status_code == 400, query
    assert client.get(API).status_code == 401
    settings.ELASTICSEARCH_URL = "http://127.0.0.1:1"
    search.client.cache_clear()
    try:
        resp = client.get(f"{API}?q=cat", headers=as_user())
        assert resp.status_code == 503
        assert resp.json()["error"]["code"] == "SEARCH_UNAVAILABLE"
    finally:
        search.client.cache_clear()


# ---------------------------------------------------------------------------------- rebuild


def test_a_rebuild_swaps_the_alias_and_catches_up_with_changes_during_it(
    client: Client,
    as_user: Any,
    catalog: dict[str, Drawing],
    es: Any,
    make_drawing: Any,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    old = list(es.indices.get_alias(name=search.ALIAS))
    real_bulk = search.streaming_bulk  # type: ignore[attr-defined]

    def bulk_then_changes(*args: Any, **kwargs: Any) -> Any:
        yield from real_bulk(*args, **kwargs)
        # After the bulk read Postgres, before the alias moves: a drawing is approved and a
        # listing is created (the indexer would write both to the old index).
        approved(make_drawing, "kangaroo")
        indexer.apply(*created(catalog["elegant"].id, uuid.uuid4(), 77, T0))

    monkeypatch.setattr(search, "streaming_bulk", bulk_then_changes)
    result = search.reindex(es)
    es.indices.refresh(index=search.ALIAS)

    assert list(es.indices.get_alias(name=search.ALIAS)) == [result["index"]] != old
    assert not es.indices.exists(index=old[0])
    assert result["caught_up"] >= 2
    assert words(client.get(f"{API}?q=kangaroo", headers=as_user())) == ["kangaroo"]
    assert words(client.get(f"{API}?listed=true&sort=price_asc", headers=as_user())) == [
        "cat",
        "elephant",
        "elegant",
        "catapult",
    ]
