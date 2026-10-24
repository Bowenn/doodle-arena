package room

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	gamev1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/protocol"
	"google.golang.org/protobuf/proto"
)

// binaryConn is a socket that negotiated binary stroke frames (ADR 0025): stroke events come
// through SendStroke, everything else through Send.
type binaryConn struct {
	fakeConn
	bmu    sync.Mutex
	frames [][]byte
}

func (c *binaryConn) BinaryStrokes() bool { return true }

func (c *binaryConn) SendStroke(_, bin []byte) {
	c.bmu.Lock()
	c.frames = append(c.frames, bin)
	c.bmu.Unlock()
}

func (c *binaryConn) strokeEvents(t *testing.T) []*gamev1.StrokeBatchEvent {
	t.Helper()
	c.bmu.Lock()
	defer c.bmu.Unlock()
	var out []*gamev1.StrokeBatchEvent
	for _, b := range c.frames {
		var f gamev1.StrokeFrame
		if err := proto.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f.GetEvent())
	}
	return out
}

func TestStrokeEventsGoBinaryOnlyToSocketsThatAskedAndEverythingElseStaysJSON(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	h.join("a")
	h.join("b")
	bc := &binaryConn{fakeConn: fakeConn{uid: "c"}}
	if err := h.room.Join(context.Background(), bc, "Name c", 0); err != nil {
		t.Fatal(err)
	}

	h.input("a", protocol.Start{})
	h.advance(StartingFor)
	drawer := h.drawerIDs("a", "choosing")[0]
	as := func(uid string) Conn {
		if uid == "c" {
			return bc
		}
		return h.conns[uid]
	}
	send := func(m protocol.ClientMsg) {
		if err := h.room.Input(context.Background(), as(drawer), m); err != nil {
			t.Fatal(err)
		}
		h.sync()
	}
	send(protocol.ChooseWord{Index: 0})
	stroke := protocol.StrokeBatch{StrokeID: "s1", Points: [][]int{{10, 20, 0}, {30, 40, 16}}, Color: "#1e88e5", Width: 40}
	send(stroke)

	// The JSON sockets other than the drawer get the JSON event; the binary socket gets the
	// same event, seq and all, as a frame, and no JSON copy of it.
	var jsonSeq int64
	for _, uid := range []string{"a", "b"} {
		if uid == drawer {
			continue
		}
		evs := h.conns[uid].events("stroke_batch")
		if len(evs) != 1 {
			t.Fatalf("%s got %d JSON stroke events", uid, len(evs))
		}
		jsonSeq = evs[0].Seq
		var got CanvasBatch
		_ = json.Unmarshal(evs[0].Data.(json.RawMessage), &got)
		if got.UserID != drawer || got.StrokeID != "s1" || len(got.Points) != 2 {
			t.Fatalf("%s: JSON stroke = %+v", uid, got)
		}
	}
	if drawer != "c" {
		frames := bc.strokeEvents(t)
		if len(frames) != 1 {
			t.Fatalf("binary socket got %d frames", len(frames))
		}
		f := frames[0]
		if f.GetSeq() != jsonSeq || f.GetUserId() != drawer || f.GetStrokeId() != "s1" || f.GetColor() != "#1e88e5" ||
			f.GetWidth() != 40 || len(f.GetPoints()) != 6 || f.GetPoints()[3] != 30 || f.GetPoints()[5] != 16 {
			t.Fatalf("binary frame = %v (JSON seq %d)", f, jsonSeq)
		}
		if n := len(bc.events("stroke_batch")); n != 0 {
			t.Fatalf("binary socket also got %d JSON stroke events", n)
		}
	}
	// Every other event reaches the binary socket as JSON, as before.
	if len(bc.events("round_started")) != 1 || len(bc.events("match_started")) != 1 {
		t.Fatalf("binary socket's JSON events = %v", bc.events(""))
	}
}

func TestNoBinaryFrameIsEncodedWithoutASocketThatAsked(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	h.join("a")
	h.join("b")
	drawer, _ := h.startDrawing("a")
	h.input(drawer, protocol.StrokeBatch{StrokeID: "s1", Points: [][]int{{1, 2, 0}}, Color: "#000000", Width: 1})
	h.sync()
	if h.room.anyBinary() {
		t.Fatal("anyBinary with JSON sockets only")
	}
}
