package main

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/events/v1"
	gamev1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/game/v1"
)

//go:embed fixtures/*.json
var fixtureFS embed.FS

const producer = "fake-events"

// loadFixture parses fixtures/<name>.json (protojson) into m.
func loadFixture(name string, m proto.Message) error {
	b, err := fixtureFS.ReadFile("fixtures/" + name + ".json")
	if err != nil {
		return fmt.Errorf("read fixture %s: %w", name, err)
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return fmt.Errorf("parse fixture %s: %w", name, err)
	}
	return nil
}

// batch is one fake round and match, ready to upload and publish.
type batch struct {
	matchID      string
	strokeLog    *gamev1.StrokeLog
	strokeLogKey string
	round        *eventsv1.RoundEvent
	match        *eventsv1.MatchEvent
}

// buildBatch turns the fixtures into a consistent round and match that share matchID, the
// way the game server would produce them: the stroke log is stored under
// "<match_id>/<round_no>.binpb.gz" and RoundFinished points at it (claim check).
func buildBatch(matchID string, now time.Time, newID func() string) (*batch, error) {
	b := &batch{
		matchID:   matchID,
		strokeLog: &gamev1.StrokeLog{},
		round:     &eventsv1.RoundEvent{},
		match:     &eventsv1.MatchEvent{},
	}
	for name, m := range map[string]proto.Message{
		"stroke_log":     b.strokeLog,
		"round_finished": b.round,
		"match_finished": b.match,
	} {
		if err := loadFixture(name, m); err != nil {
			return nil, err
		}
	}

	roundStart := now.Add(-80 * time.Second)
	b.strokeLog.MatchId = matchID
	b.strokeLog.RoundStartedAt = timestamppb.New(roundStart)

	rf := b.round.GetRoundFinished()
	b.strokeLogKey = fmt.Sprintf("%s/%d.binpb.gz", matchID, rf.GetRoundNo())
	rf.MatchId = matchID
	rf.StrokeLogKey = b.strokeLogKey
	rf.StrokeCount = uint32(len(b.strokeLog.GetStrokes()))
	rf.EndedAt = timestamppb.New(now)

	mf := b.match.GetMatchFinished()
	mf.MatchId = matchID
	mf.StartedAt = timestamppb.New(roundStart.Add(-10 * time.Second))
	mf.EndedAt = timestamppb.New(now)

	for _, w := range []proto.Message{b.round, b.match} {
		meta := &eventsv1.EventMeta{
			EventId:       newID(),
			SchemaVersion: 1,
			OccurredAt:    timestamppb.New(now),
			Producer:      producer,
		}
		if err := setMeta(w, meta); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// gzipStrokeLog serializes a stroke log the way it is stored in the round-logs bucket.
func gzipStrokeLog(log *gamev1.StrokeLog) ([]byte, error) {
	raw, err := proto.Marshal(log)
	if err != nil {
		return nil, fmt.Errorf("marshal stroke log: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip stroke log: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip stroke log: %w", err)
	}
	return buf.Bytes(), nil
}

// gunzipStrokeLog is the reverse of gzipStrokeLog; consumers do the same.
func gunzipStrokeLog(b []byte) (*gamev1.StrokeLog, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("gunzip stroke log: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("gunzip stroke log: %w", err)
	}
	log := &gamev1.StrokeLog{}
	if err := proto.Unmarshal(raw, log); err != nil {
		return nil, fmt.Errorf("unmarshal stroke log: %w", err)
	}
	return log, nil
}
