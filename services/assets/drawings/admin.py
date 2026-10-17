"""The Django admin is the moderation console (design doc 4.5). Drawings are read-only here:
the pipeline creates them, and a moderator only approves or rejects the ones in the review
queue, through the same guarded status change the pipeline uses (drawings.moderation)."""

from typing import Any

from django.contrib import admin, messages
from django.db.models import QuerySet
from django.http import HttpRequest

from drawings import moderation
from drawings.models import DecidedBy, Drawing, ReviewQueue, Status

admin.site.site_header = "Doodle Arena moderation"
admin.site.site_title = "Doodle Arena moderation"
admin.site.index_title = "Moderation"

_FIELDS = [f.name for f in Drawing._meta.fields]


class ReadOnlyAdmin(admin.ModelAdmin):  # type: ignore[type-arg]
    """Shows rows and never changes them through a form."""

    readonly_fields = _FIELDS

    def has_add_permission(self, request: HttpRequest) -> bool:
        return False

    def has_change_permission(self, request: HttpRequest, obj: Any = None) -> bool:
        return False

    def has_delete_permission(self, request: HttpRequest, obj: Any = None) -> bool:
        return False


@admin.register(Drawing)
class DrawingAdmin(ReadOnlyAdmin):
    list_display = ("id", "match_id", "round_no", "word", "status", "decided_by", "created_at")
    list_filter = ("status", "decided_by")
    search_fields = ("=id", "=match_id", "word")
    ordering = ("-id",)


@admin.register(ReviewQueue)
class ReviewQueueAdmin(ReadOnlyAdmin):
    list_display = ("id", "match_id", "round_no", "word", "decision_reason", "created_at")
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
