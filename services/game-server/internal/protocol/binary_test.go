package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gamev1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/game/v1"
	"google.golang.org/protobuf/proto"
)

func frame(t *testing.T, in *gamev1.StrokeBatchIn) []byte {
	t.Helper()
	b, err := proto.Marshal(&gamev1.StrokeFrame{Frame: &gamev1.StrokeFrame_Batch{Batch: in}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestABinaryStrokeBatchDecodesToTheSameMessageAsJSON(t *testing.T) {
	fromJSON, err := DecodeClient([]byte(`{"type":"stroke_batch","data":{"stroke_id":"s_1","points":[[1,2,0],[10000,9999,600000]],"color":"#1e88e5","width":200,"done":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	fromBinary, err := DecodeStrokeFrame(frame(t, &gamev1.StrokeBatchIn{
		StrokeId: "s_1", Points: []uint32{1, 2, 0, 10000, 9999, 600000}, Color: "#1e88e5", Width: 200, Done: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a, b := fromJSON.(StrokeBatch), fromBinary
	if a.StrokeID != b.StrokeID || a.Color != b.Color || a.Width != b.Width || a.Done != b.Done || len(a.Points) != len(b.Points) {
		t.Fatalf("json %+v, binary %+v", a, b)
	}
	for i := range a.Points {
		for j := range 3 {
			if a.Points[i][j] != b.Points[i][j] {
				t.Fatalf("point %d: json %v, binary %v", i, a.Points[i], b.Points[i])
			}
		}
	}
}

func TestABinaryStrokeBatchHasTheJSONLimits(t *testing.T) {
	ok := func() *gamev1.StrokeBatchIn {
		return &gamev1.StrokeBatchIn{StrokeId: "s1", Points: []uint32{1, 2, 3}, Color: "#000000", Width: 1}
	}
	many := make([]uint32, 3*(MaxPointsPerMsg+1))
	tests := map[string][]byte{
		"not protobuf":          {0xff, 0xff, 0xff},
		"an event, not a batch": mustMarshal(t, &gamev1.StrokeFrame{Frame: &gamev1.StrokeFrame_Event{Event: &gamev1.StrokeBatchEvent{}}}),
		"empty frame":           mustMarshal(t, &gamev1.StrokeFrame{}),
		"half a point":          frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Points = []uint32{1, 2}; return m }()),
		"no points":             frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Points = nil; return m }()),
		"too many points":       frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Points = many; return m }()),
		"x off the canvas":      frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Points = []uint32{10001, 0, 0}; return m }()),
		"t_ms too late":         frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Points = []uint32{0, 0, 600001}; return m }()),
		"width 0":               frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Width = 0; return m }()),
		"width 4 billion":       frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Width = 1<<32 - 1; return m }()),
		"bad color":             frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.Color = "red"; return m }()),
		"bad stroke id":         frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.StrokeId = "a b"; return m }()),
		// Marshal refuses invalid UTF-8, so the bytes are corrupted after it.
		"invalid UTF-8 id": bytes.Replace(frame(t, func() *gamev1.StrokeBatchIn { m := ok(); m.StrokeId = "QQ"; return m }()),
			[]byte("QQ"), []byte{0xff, 0xfe}, 1),
	}
	for name, raw := range tests {
		if _, err := DecodeStrokeFrame(raw); !errors.Is(err, ErrBadMessage) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := DecodeStrokeFrame(frame(t, ok())); err != nil {
		t.Fatal(err)
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAStrokeEventFrameCarriesTheEvent(t *testing.T) {
	b := EncodeStrokeEvent(42, "u_ada", StrokeBatch{StrokeID: "s1", Points: [][]int{{1, 2, 3}, {4, 5, 6}}, Color: "#abcdef", Width: 7, Done: true})
	var f gamev1.StrokeFrame
	if err := proto.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	e := f.GetEvent()
	if e.GetSeq() != 42 || e.GetUserId() != "u_ada" || e.GetStrokeId() != "s1" || e.GetColor() != "#abcdef" || e.GetWidth() != 7 || !e.GetDone() {
		t.Fatalf("event = %v", e)
	}
	if got := e.GetPoints(); len(got) != 6 || got[0] != 1 || got[5] != 6 {
		t.Fatalf("points = %v", got)
	}
}

// The sizes behind ADR 0025: one batch of 3 points (a 50 ms batch at 60 points a second) as a
// JSON event and as a binary frame.
func BenchmarkStrokeEventEncoding(b *testing.B) {
	m := StrokeBatch{StrokeID: "t1761234567890_12_34", Points: [][]int{{4821, 7302, 1450}, {4833, 7311, 1467}, {4850, 7322, 1483}}, Color: "#1e88e5", Width: 40}
	ev := Event{Seq: 1234, Type: TypeStrokeBatch, Data: struct {
		UserID string `json:"user_id"`
		StrokeBatch
	}{"u_0123456789abcdef", m}}
	b.Run("json", func(b *testing.B) {
		for b.Loop() {
			_ = Marshal("event", ev)
		}
		b.ReportMetric(float64(len(Marshal("event", ev))), "bytes")
	})
	b.Run("pb", func(b *testing.B) {
		for b.Loop() {
			_ = EncodeStrokeEvent(1234, "u_0123456789abcdef", m)
		}
		b.ReportMetric(float64(len(EncodeStrokeEvent(1234, "u_0123456789abcdef", m))), "bytes")
	})
}

// The web app and k6 have their own small codec for these frames (ADR 0025). Its tests read the
// bytes Go produces from web/src/game/testdata/stroke-frames.json; this test keeps that file
// true. UPDATE_FIXTURES=1 rewrites it.
func TestWebStrokeFixturesMatchGo(t *testing.T) {
	batch := &gamev1.StrokeBatchIn{StrokeId: "t1761234567890_12_34", Points: []uint32{0, 10000, 0, 4821, 7302, 1450, 127, 128, 600000},
		Color: "#1e88e5", Width: 40, Done: true}
	event := StrokeBatch{StrokeID: batch.GetStrokeId(), Points: [][]int{{0, 10000, 0}, {4821, 7302, 1450}, {127, 128, 600000}},
		Color: "#1e88e5", Width: 40, Done: false}
	want := map[string]any{
		"batch": map[string]any{"stroke_id": batch.GetStrokeId(), "points": [][]int{{0, 10000, 0}, {4821, 7302, 1450}, {127, 128, 600000}},
			"color": "#1e88e5", "width": 40, "done": true, "hex": hex.EncodeToString(frame(t, batch))},
		"event": map[string]any{"seq": 1234567, "user_id": "u_0123456789abcdef", "stroke_id": event.StrokeID, "points": event.Points,
			"color": event.Color, "width": event.Width, "done": event.Done,
			"hex": hex.EncodeToString(EncodeStrokeEvent(1234567, "u_0123456789abcdef", event))},
	}
	got, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("..", "..", "..", "..", "web", "src", "game", "testdata", "stroke-frames.json")
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // a test fixture
			t.Fatal(err)
		}
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run UPDATE_FIXTURES=1 go test ./internal/protocol)", err)
	}
	if !bytes.Equal(have, got) {
		t.Fatalf("%s is out of date: run UPDATE_FIXTURES=1 go test ./internal/protocol", path)
	}
}
