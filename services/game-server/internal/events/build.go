package events

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"time"

	eventsv1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/events/v1"
	gamev1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/room"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Topics from the event catalog (design doc, section 6.2). Both are keyed by room_id.
const (
	TopicRounds  = "game.rounds.v1"
	TopicMatches = "game.matches.v1"
)

const (
	producerName      = "game-server"
	schemaVersion     = 1
	strokeLogVersion  = 1
	strokeLogMIMEType = "application/gzip"
)

// StrokeLogKey is the object key of a round's stroke log in the round-logs bucket.
func StrokeLogKey(matchID string, roundNo int) string {
	return fmt.Sprintf("%s/%d.binpb.gz", matchID, roundNo)
}

// StrokeLog turns a finished round's canvas into the stored stroke log. The room keeps stroke
// batches; a stroke is every batch with the same stroke_id, in the order the room processed
// them. Undone strokes are already gone from the canvas, so none is marked undone.
func StrokeLog(r room.FinishedRound) *gamev1.StrokeLog {
	var strokes []*gamev1.Stroke
	byID := map[string]*gamev1.Stroke{}
	for _, b := range r.Canvas {
		s := byID[b.StrokeID]
		if s == nil {
			s = &gamev1.Stroke{StrokeId: b.StrokeID, UserId: b.UserID, Color: b.Color, Width: uint32(b.Width)}
			byID[b.StrokeID] = s
			strokes = append(strokes, s)
		}
		for _, p := range b.Points {
			s.Points = append(s.Points, &gamev1.Point{X: uint32(p[0]), Y: uint32(p[1]), TMs: uint32(p[2])})
		}
		// The time the room received the stroke's last batch, by the server's clock.
		s.ServerReceivedMs = uint32(max(0, b.ReceivedAt.Sub(r.DrawStart).Milliseconds()))
	}
	return &gamev1.StrokeLog{
		SchemaVersion: strokeLogVersion, MatchId: r.MatchID, RoomId: r.RoomID, RoundNo: uint32(r.RoundNo),
		CanvasWidth: room.CanvasWidth, CanvasHeight: room.CanvasHeight,
		RoundStartedAt: timestamppb.New(r.DrawStart), Strokes: strokes,
	}
}

// gzipProto serializes m and gzips it.
func gzipProto(m proto.Message) ([]byte, error) {
	raw, err := proto.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	return buf.Bytes(), nil
}

func meta(eventID, eventType string, at time.Time) *eventsv1.EventMeta {
	return &eventsv1.EventMeta{
		EventId: eventID, EventType: eventType, SchemaVersion: schemaVersion,
		OccurredAt: timestamppb.New(at), Producer: producerName,
	}
}

// RoundEvent is the game.rounds.v1 message for a finished round whose stroke log is stored
// at key (the claim check).
func RoundEvent(eventID string, r room.FinishedRound, key string, strokes int) *eventsv1.RoundEvent {
	return &eventsv1.RoundEvent{Event: &eventsv1.RoundEvent_RoundFinished{RoundFinished: &eventsv1.RoundFinished{
		Meta: meta(eventID, "RoundFinished", r.EndedAt), MatchId: r.MatchID, RoomId: r.RoomID,
		RoundNo: uint32(r.RoundNo), Word: r.Word, DrawerIds: r.DrawerIDs, StrokeLogKey: key,
		StrokeCount: uint32(strokes), CanvasWidth: room.CanvasWidth, CanvasHeight: room.CanvasHeight,
		EndedAt: timestamppb.New(r.EndedAt),
	}}}
}

// MatchEvent is the game.matches.v1 message for a finished match. It has placements but no
// coin amounts: the marketplace decides rewards.
func MatchEvent(eventID string, m room.FinishedMatch) *eventsv1.MatchEvent {
	results := make([]*eventsv1.PlayerResult, 0, len(m.Results))
	for _, r := range m.Results {
		results = append(results, &eventsv1.PlayerResult{UserId: r.UserID, Score: uint32(r.Score), Placement: uint32(r.Placement)})
	}
	return &eventsv1.MatchEvent{Event: &eventsv1.MatchEvent_MatchFinished{MatchFinished: &eventsv1.MatchFinished{
		Meta: meta(eventID, "MatchFinished", m.EndedAt), MatchId: m.MatchID, RoomId: m.RoomID,
		Mode: gameMode(m.Mode), StartedAt: timestamppb.New(m.StartedAt), EndedAt: timestamppb.New(m.EndedAt),
		Results: results,
	}}}
}

func gameMode(mode string) gamev1.GameMode {
	if mode == "classic" {
		return gamev1.GameMode_GAME_MODE_CLASSIC
	}
	return gamev1.GameMode_GAME_MODE_UNSPECIFIED
}
