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


def test_a_matchs_drawings(client: Client, as_user: Any, make_drawing: Any) -> None:
    approved(make_drawing, match_id="m_one", round_no=1)
    mine = make_drawing(
        match_id="m_one", round_no=2, author_ids=["u_cyd"], initial_owner_id="u_cyd"
    )
    approved(make_drawing, match_id="m_two", round_no=1)

    as_cyd = client.get(f"{API}?match_id=m_one", headers=as_user("u_cyd")).json()["drawings"]
    as_other = client.get(f"{API}?match_id=m_one", headers=as_user("u_ada")).json()["drawings"]

    assert {d["round_no"] for d in as_cyd} == {1, 2}
    assert [d["round_no"] for d in as_other] == [1]
    assert str(mine.id) not in str(as_other)


def test_a_lookup_by_ids_answers_in_the_order_asked(
    client: Client, as_user: Any, make_drawing: Any
) -> None:
    first = approved(make_drawing, round_no=1)
    second = approved(make_drawing, round_no=2)
    hidden = make_drawing(round_no=3, author_ids=["u_cyd"], initial_owner_id="u_cyd")
    unknown = "0193a4f0-0000-7000-8000-00000000abcd"

    ids = f"{second.id},{unknown},{first.id},{hidden.id},{second.id}"
    as_other = client.get(f"{API}?ids={ids}", headers=as_user("u_ada")).json()
    as_cyd = client.get(f"{API}?ids={ids}", headers=as_user("u_cyd")).json()

    assert [d["id"] for d in as_other["drawings"]] == [str(second.id), str(first.id)]
    assert as_other["next_cursor"] is None
    assert [d["id"] for d in as_cyd["drawings"]] == [str(second.id), str(first.id), str(hidden.id)]
    assert (
        as_other["drawings"][0]["thumb_url"] is None
        or "http" in as_other["drawings"][0]["thumb_url"]
    )


@pytest.mark.parametrize(
    "query",
    [
        "ids=",
        "ids=nope",
        "ids=" + ",".join(["0193a4f0-0000-7000-8000-000000000001"] * 51),
        "limit=0",
        "limit=51",
        "limit=x",
        "cursor=nope",
        "author_id=" + "u" * 65,
        "match_id=" + "m" * 65,
    ],
)
def test_bad_queries_are_refused(client: Client, as_user: Any, query: str) -> None:
    resp = client.get(f"{API}?{query}", headers=as_user("u_x"))

    assert resp.status_code == 400
    assert resp.json()["error"]["code"] == "INVALID_REQUEST"


def test_a_post_is_not_allowed(as_user: Any) -> None:
    # A client that enforces CSRF like a browser does: the API answers 405 in JSON, not with
    # Django's CSRF page, because it never relies on cookies.
    resp = Client(enforce_csrf_checks=True).post(API, headers=as_user("u_x"))

    assert resp.status_code == 405
    assert resp.json()["error"]["code"] == "METHOD_NOT_ALLOWED"
    assert resp["Allow"] == "GET, HEAD"


@pytest.mark.parametrize(
    "path",
    [
        "/api/assets/nothing-here",
        f"{API}/not-a-uuid",
        f"{API}/01a14d4c-3700-714c-97af-6822304866ec/x",
    ],
)
def test_unknown_api_paths_answer_in_json(client: Client, path: str) -> None:
    resp = client.get(path)

    assert resp.status_code == 404
    assert resp["Content-Type"] == "application/json"
    assert resp.json()["error"]["code"] == "NOT_FOUND"


def test_the_admin_keeps_djangos_404_page(client: Client) -> None:
    resp = client.get("/admin/nothing-here/")

    assert resp.status_code in (302, 404)  # signed out: the sign-in redirect comes first
    assert resp["Content-Type"].startswith("text/html")


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


def test_image_urls_stay_the_same_for_a_while_so_browsers_can_cache_them(
    client: Client, as_user: Any, make_drawing: Any
) -> None:
    storage.reset_clients_for_tests()
    d = approved(make_drawing)

    first = client.get(f"{API}/{d.id}", headers=as_user("u_a")).json()
    again = client.get(f"{API}/{d.id}", headers=as_user("u_b")).json()

    assert (again["image_url"], again["thumb_url"]) == (first["image_url"], first["thumb_url"])


def test_presigned_urls_are_reused_then_signed_again(monkeypatch: pytest.MonkeyPatch) -> None:
    import time

    signed: list[str] = []

    class Signer:
        def generate_presigned_url(self, op: str, Params: dict[str, str], ExpiresIn: int) -> str:
            signed.append(Params["Key"])
            return f"url-{len(signed)}"

    storage.reset_clients_for_tests()
    monkeypatch.setattr(storage, "_client", lambda endpoint: Signer())

    assert storage.presigned_url("k") == "url-1"
    assert storage.presigned_url("k") == "url-1"
    assert storage.presigned_url("other") == "url-2"

    later = time.monotonic() + storage.PRESIGN_REUSE_SECONDS + 1
    monkeypatch.setattr("drawings.storage.time.monotonic", lambda: later)
    assert storage.presigned_url("k") == "url-3"  # still at least 5 minutes of validity each


def test_an_empty_endpoint_signs_for_s3_itself(settings: Any) -> None:
    """In AWS (ADR 0003) the endpoint is empty and addressing virtual-hosted: URLs point at S3."""
    settings.S3_PUBLIC_ENDPOINT = ""
    settings.S3_REGION = "us-west-2"
    settings.S3_PATH_STYLE = False
    settings.S3_ACCESS_KEY = "AKIDEXAMPLE"
    settings.S3_SECRET_KEY = "example"
    settings.DRAWINGS_BUCKET = "doodle-drawings"
    storage.reset_clients_for_tests()
    try:
        url = storage.presigned_url("sha256/abc.png")
    finally:
        storage.reset_clients_for_tests()

    assert url.startswith("https://doodle-drawings.s3.us-west-2.amazonaws.com/sha256/abc.png?")
