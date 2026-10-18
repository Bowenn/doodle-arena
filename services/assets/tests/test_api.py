"""The drawings REST API: authentication, who sees what, keyset pagination, replays, and
presigned image URLs that really load from object storage."""

import urllib.request
from typing import Any

import pytest
from django.test import Client

from drawings import moderation, pipeline, storage
from drawings.models import DecidedBy, Drawing, Status
from tests.conftest import IMAGE, stroke_log_bytes

pytestmark = pytest.mark.django_db
API = "/api/assets/drawings"


@pytest.fixture
def as_user(signer: Any) -> Any:
    def headers(user_id: str) -> dict[str, str]:
        return {"Authorization": f"Bearer {signer.token(user_id, ['player'])}"}

    return headers


def approved(make_drawing: Any, **fields: Any) -> Drawing:
    d: Drawing = make_drawing(**{**IMAGE, **fields})
    moderation.decide(d.id, approve=True, by=DecidedBy.AI)
    d.refresh_from_db()
    return d


def test_every_endpoint_needs_a_token(client: Client, make_drawing: Any) -> None:
    d = approved(make_drawing)
    for url in (API, f"{API}/{d.id}", f"{API}/{d.id}/replay"):
        resp = client.get(url)
        assert resp.status_code == 401
        assert resp.json()["error"]["code"] == "UNAUTHENTICATED"
    assert client.get(API, headers={"Authorization": "Bearer nope"}).status_code == 401


def test_an_approved_drawing_is_visible_to_everyone(
    client: Client, as_user: Any, make_drawing: Any
) -> None:
    d = approved(make_drawing)

    resp = client.get(f"{API}/{d.id}", headers=as_user("u_stranger"))

    assert resp.status_code == 200
    body = resp.json()
    assert body["id"] == str(d.id)
    assert body["status"] == "approved"
    assert body["author_ids"] == ["u_ada", "u_bob"]
    assert body["word"] == "cat"
    assert "X-Amz-Signature=" in body["image_url"]
    assert "private" in resp["Cache-Control"]
    assert "decision_reason" not in body


def test_other_drawings_are_visible_only_to_their_authors(
    client: Client, as_user: Any, make_drawing: Any
) -> None:
    d = make_drawing(**IMAGE)
    moderation.send_to_review(d.id, reason="AI unsure: blurry")

    mine = client.get(f"{API}/{d.id}", headers=as_user("u_bob"))
    assert mine.status_code == 200
    assert mine.json()["status"] == "review"
    assert "reason" not in str(mine.json())

    theirs = client.get(f"{API}/{d.id}", headers=as_user("u_stranger"))
    assert theirs.status_code == 404
    assert theirs.json()["error"]["code"] == "DRAWING_NOT_FOUND"
    assert client.get(f"{API}/{d.id}/replay", headers=as_user("u_stranger")).status_code == 404


def test_the_gallery_lists_approved_drawings_newest_first_with_a_cursor(
    client: Client, as_user: Any, make_drawing: Any
) -> None:
    ids = [str(approved(make_drawing, round_no=i).id) for i in range(1, 6)]
    hidden = make_drawing(round_no=9)  # still processing
    assert hidden.status == Status.PROCESSING

    first = client.get(f"{API}?limit=2", headers=as_user("u_stranger")).json()
    second = client.get(
        f"{API}?limit=2&cursor={first['next_cursor']}", headers=as_user("u_x")
    ).json()
    third = client.get(
        f"{API}?limit=2&cursor={second['next_cursor']}", headers=as_user("u_x")
    ).json()

    listed = [d["id"] for page in (first, second, third) for d in page["drawings"]]
    assert listed == list(reversed(ids))
    assert third["next_cursor"] is None


def test_an_authors_list_includes_their_own_undecided_drawings(
    client: Client, as_user: Any, make_drawing: Any
) -> None:
    approved(make_drawing, round_no=1, author_ids=["u_cyd"], initial_owner_id="u_cyd")
    pending = make_drawing(round_no=2, author_ids=["u_cyd"], initial_owner_id="u_cyd")
    approved(make_drawing, round_no=3)  # someone else's

    own = client.get(f"{API}?author_id=u_cyd", headers=as_user("u_cyd")).json()["drawings"]
    other = client.get(f"{API}?author_id=u_cyd", headers=as_user("u_ada")).json()["drawings"]

    assert {d["round_no"] for d in own} == {1, 2}
    assert [d["round_no"] for d in other] == [1]
    assert str(pending.id) not in str(other)


@pytest.mark.parametrize(
    "query", ["limit=0", "limit=51", "limit=x", "cursor=nope", "author_id=" + "u" * 65]
)
def test_bad_queries_are_refused(client: Client, as_user: Any, query: str) -> None:
    resp = client.get(f"{API}?{query}", headers=as_user("u_x"))

    assert resp.status_code == 400
    assert resp.json()["error"]["code"] == "INVALID_REQUEST"


def test_a_post_is_not_allowed(client: Client, as_user: Any) -> None:
    assert client.post(API, headers=as_user("u_x")).status_code == 405


@pytest.mark.usefixtures("stores")
def test_the_replay_and_the_presigned_images_load(
    client: Client, as_user: Any, make_drawing: Any, settings: Any
) -> None:
    settings.S3_PUBLIC_ENDPOINT = settings.S3_ENDPOINT  # the test container is the public address
    storage.reset_clients_for_tests()
    storage.s3().put_object(
        Bucket="round-logs", Key="m_api/1.binpb.gz", Body=stroke_log_bytes("m_api")
    )
    d = make_drawing(match_id="m_api", stroke_log_key="m_api/1.binpb.gz", stroke_count=1)
    pipeline.prepare(d.id)
    moderation.decide(d.id, approve=True, by=DecidedBy.AI)

    body = client.get(f"{API}/{d.id}", headers=as_user("u_stranger")).json()
    for url in (body["image_url"], body["thumb_url"]):
        with urllib.request.urlopen(
            url, timeout=5
        ) as resp:  # no credentials: the signature is enough
            assert resp.status == 200
            assert resp.read().startswith(b"\x89PNG")

    replay = client.get(f"{API}/{d.id}/replay", headers=as_user("u_stranger")).json()
    assert replay["drawing_id"] == str(d.id)
    assert replay["schema_version"] == 1
    assert replay["strokes"][0]["points"][0] == [1000, 1000, 0]
    assert "_id" not in replay
