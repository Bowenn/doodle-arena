"""The Django admin is the moderation console (design doc 4.5). Drawings are read-only here:
the pipeline creates them, and a moderator only approves or rejects the ones in the review
queue, through the same guarded status change the pipeline uses (drawings.moderation)."""

import uuid
from typing import Any

from django.contrib import admin, messages
from django.db.models import QuerySet
from django.http import Http404, HttpRequest, HttpResponse
from django.urls import URLPattern, path, reverse
from django.utils.html import format_html

from drawings import moderation, storage
from drawings.models import DecidedBy, Drawing, ReviewQueue, Status

admin.site.site_header = "Doodle Arena moderation"
admin.site.site_title = "Doodle Arena moderation"
admin.site.index_title = "Moderation"

_FIELDS = [f.name for f in Drawing._meta.fields]


class ReadOnlyAdmin(admin.ModelAdmin):  # type: ignore[type-arg]
    """Shows rows and never changes them through a form, with the drawing's picture."""

    readonly_fields = ["preview", *_FIELDS]

    @admin.display(description="drawing")
    def thumbnail(self, obj: Drawing) -> str:
        if not obj.thumb_key:
            return "not rendered yet"
        url = reverse("admin:drawings_image", args=[obj.pk, "thumb"])
        return format_html('<img src="{}" alt="" height="96" loading="lazy">', url)

    @admin.display(description="image")
    def preview(self, obj: Drawing) -> str:
        if not obj.image_key:
            return "not rendered yet"
        url = reverse("admin:drawings_image", args=[obj.pk, "full"])
        return format_html('<img src="{}" alt="" style="max-width: 512px">', url)

    def has_add_permission(self, request: HttpRequest) -> bool:
        return False

    def has_change_permission(self, request: HttpRequest, obj: Any = None) -> bool:
        return False

    def has_delete_permission(self, request: HttpRequest, obj: Any = None) -> bool:
        return False


@admin.register(Drawing)
class DrawingAdmin(ReadOnlyAdmin):
    list_display = (
        "thumbnail",
        "id",
        "match_id",
        "round_no",
        "word",
        "status",
        "decided_by",
        "created_at",
    )
    list_filter = ("status", "decided_by")
    search_fields = ("=id", "=match_id", "word")
    ordering = ("-id",)

    def get_urls(self) -> list[URLPattern]:
        image = path(
            "<uuid:drawing_id>/<str:size>.png",
            self.admin_site.admin_view(self.image_view),
            name="drawings_image",
        )
        return [image, *super().get_urls()]

    def image_view(self, request: HttpRequest, drawing_id: uuid.UUID, size: str) -> HttpResponse:
        """Streams a drawing's image from the bucket to a signed-in moderator. The browser can't
        reach the object store's internal address, and this keeps the admin's access check."""
        if size not in ("thumb", "full") or not self.has_view_permission(request):
            raise Http404
        drawing = Drawing.objects.filter(pk=drawing_id).first()
        key = drawing and (drawing.thumb_key if size == "thumb" else drawing.image_key)
        if not key:
            raise Http404
        response = HttpResponse(storage.get_image(key), content_type=storage.PNG)
        # Content-addressed, so it never changes; private, because only moderators may see it.
        response["Cache-Control"] = "private, max-age=86400, immutable"
        return response


@admin.register(ReviewQueue)
class ReviewQueueAdmin(ReadOnlyAdmin):
    list_display = (
        "thumbnail",
        "id",
        "match_id",
        "round_no",
        "word",
        "decision_reason",
        "created_at",
    )
    ordering = ("created_at",)  # oldest first, matching the partial index
    actions = ("approve", "reject")

    def get_queryset(self, request: HttpRequest) -> QuerySet[Any]:
        return super().get_queryset(request).filter(status=Status.REVIEW)

    def has_moderate_permission(self, request: HttpRequest) -> bool:
        return request.user.has_perm("drawings.moderate_drawing")

    @admin.action(description="Approve selected drawings", permissions=["moderate"])
    def approve(self, request: HttpRequest, queryset: QuerySet[Any]) -> None:
        self._decide(request, queryset, approve=True)

    @admin.action(description="Reject selected drawings", permissions=["moderate"])
    def reject(self, request: HttpRequest, queryset: QuerySet[Any]) -> None:
        self._decide(request, queryset, approve=False)

    def _decide(self, request: HttpRequest, queryset: QuerySet[Any], *, approve: bool) -> None:
        verb = "approved" if approve else "rejected"
        assert request.user.is_authenticated
        decided = skipped = 0
        for drawing_id in queryset.values_list("id", flat=True):
            if moderation.decide(
                drawing_id,
                approve=approve,
                by=DecidedBy.MODERATOR,
                moderator=request.user,
                reason=f"{verb} by a moderator",
            ):
                decided += 1
            else:
                skipped += 1
        self.message_user(request, f"{decided} drawing(s) {verb}.", messages.SUCCESS)
        if skipped:
            self.message_user(
                request,
                f"{skipped} drawing(s) were already decided by someone else.",
                messages.WARNING,
            )
