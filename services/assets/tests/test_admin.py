"""The moderation queue in the Django admin."""

import uuid
from typing import Any

import pytest
from django.contrib.auth.models import Permission, User
from django.test import Client

from drawings import moderation
from drawings.models import DecidedBy, Drawing, Status
from sso.views import sync_user
from tests.conftest import IMAGE

pytestmark = pytest.mark.django_db

QUEUE = "/admin/drawings/reviewqueue/"


@pytest.fixture
def moderator(client: Client) -> User:
    from config.authn import Claims

    user = sync_user(Claims(user_id="u_mod", roles=["moderator"], name="Mo"))
    client.force_login(user)
    return user


def queued(make_drawing: Any, **fields: Any) -> Drawing:
    drawing: Drawing = make_drawing(**{**IMAGE, **fields})
    moderation.send_to_review(drawing.id, reason="moderation failed after 4 attempts")
    return drawing


def test_queue_lists_only_drawings_in_review(
    client: Client, moderator: User, make_drawing: Any
) -> None:
    waiting = queued(make_drawing, round_no=1, word="waiting")
    make_drawing(round_no=2, word="processing")
    done = make_drawing(round_no=3, word="done", **IMAGE)
    moderation.decide(done.id, approve=True, by=DecidedBy.AI)

    page = client.get(QUEUE).content.decode()

    assert "waiting" in page
    assert "processing" not in page.split("<tbody>")[1]
    assert str(done.id) not in page
    assert str(waiting.id) in page


def test_moderator_approves_and_rejects_from_the_queue(
    client: Client, moderator: User, make_drawing: Any
) -> None:
    a = queued(make_drawing, round_no=1)
    b = queued(make_drawing, round_no=2)
    c = queued(make_drawing, round_no=3)

    client.post(QUEUE, {"action": "approve", "_selected_action": [str(a.id), str(b.id)]})
    client.post(QUEUE, {"action": "reject", "_selected_action": [str(c.id)]})

    for d, status in [(a, Status.APPROVED), (b, Status.APPROVED), (c, Status.REJECTED)]:
        d.refresh_from_db()
        assert d.status == status
        assert d.decided_by == DecidedBy.MODERATOR
        assert d.moderator == moderator
    assert not Drawing.objects.filter(status=Status.REVIEW).exists()


def test_already_decided_drawings_are_reported_not_changed(
    client: Client, moderator: User, make_drawing: Any
) -> None:
    d = queued(make_drawing)
    other = User.objects.create(username="u_other", is_staff=True)
    # Another moderator decides between this one loading the page and clicking.
    moderation.decide(d.id, approve=False, by=DecidedBy.MODERATOR, moderator=other)

    # The stale page still selects it. The queue no longer lists it, so nothing is decided.
    resp = client.post(QUEUE, {"action": "approve", "_selected_action": [str(d.id)]}, follow=True)

    d.refresh_from_db()
    assert d.status == Status.REJECTED
    assert d.moderator == other
    assert resp.status_code == 200


def test_drawings_cannot_be_edited_added_or_deleted(
    client: Client, moderator: User, make_drawing: Any
) -> None:
    d = queued(make_drawing)

    assert client.get("/admin/drawings/drawing/add/").status_code == 403
    assert client.post(f"/admin/drawings/drawing/{d.id}/delete/").status_code == 403
    client.post(f"/admin/drawings/drawing/{d.id}/change/", {"word": "dog"})
    d.refresh_from_db()
    assert d.word == "cat"


def test_staff_without_the_moderate_permission_has_no_actions(
    client: Client, make_drawing: Any
) -> None:
    viewer = User.objects.create(username="u_viewer", is_staff=True)
    viewer.user_permissions.set(
        Permission.objects.filter(codename__in=["view_reviewqueue", "view_drawing"])
    )
    client.force_login(viewer)
    d = queued(make_drawing)

    client.post(QUEUE, {"action": "approve", "_selected_action": [str(d.id)]})

    d.refresh_from_db()
    assert d.status == Status.REVIEW


def test_admin_static_files_are_served(client: Client) -> None:
    resp = client.get("/admin/static/admin/css/base.css")

    assert resp.status_code == 200


@pytest.mark.usefixtures("stores")
def test_moderators_see_the_drawing(client: Client, moderator: User, make_drawing: Any) -> None:
    from drawings import pipeline, storage
    from tests.conftest import stroke_log_bytes

    storage.s3().put_object(
        Bucket="round-logs", Key="m_pic/1.binpb.gz", Body=stroke_log_bytes("m_pic")
    )
    d = make_drawing(match_id="m_pic", stroke_log_key="m_pic/1.binpb.gz", stroke_count=1)
    pipeline.prepare(d.id)
    moderation.send_to_review(d.id)

    page = client.get(QUEUE).content.decode()
    thumb = f"/admin/drawings/drawing/{d.id}/thumb.png"
    assert f'src="{thumb}"' in page

    resp = client.get(thumb)
    assert resp.status_code == 200
    assert resp["Content-Type"] == "image/png"
    assert resp.content.startswith(b"\x89PNG")
    assert "private" in resp["Cache-Control"]
    assert client.get(f"/admin/drawings/drawing/{d.id}/full.png").status_code == 200
    assert client.get(f"/admin/drawings/drawing/{d.id}/huge.png").status_code == 404
    assert client.get(f"/admin/drawings/drawing/{uuid.uuid7()}/thumb.png").status_code == 404

    client.logout()
    assert client.get(thumb).status_code == 302  # to the sign-in
