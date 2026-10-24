package protocol

import (
	"fmt"

	gamev1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/game/v1"
	"google.golang.org/protobuf/proto"
)

// StrokesSubprotocol is the WebSocket subprotocol a client asks for to send and receive stroke
// batches as binary frames (doodle.game.v1.StrokeFrame, ADR 0025). Everything else on such a
// socket stays JSON text; without it, a binary frame closes the socket with 1003.
const StrokesSubprotocol = "doodle.strokes.pb"

// DecodeStrokeFrame parses a client's binary frame. It must be a StrokeFrame carrying a batch,
// which is checked exactly like the JSON stroke_batch.
func DecodeStrokeFrame(raw []byte) (StrokeBatch, error) {
	var f gamev1.StrokeFrame
	if err := proto.Unmarshal(raw, &f); err != nil {
		return StrokeBatch{}, fmt.Errorf("%w: not a StrokeFrame", ErrBadMessage)
	}
	in := f.GetBatch()
	if in == nil {
		return StrokeBatch{}, fmt.Errorf("%w: a client's binary frame is a stroke batch", ErrBadMessage)
	}
	flat := in.GetPoints()
	if len(flat)%3 != 0 {
		return StrokeBatch{}, fmt.Errorf("%w: a point is [x, y, t_ms]", ErrBadMessage)
	}
	if len(flat) > 3*MaxPointsPerMsg {
		return StrokeBatch{}, fmt.Errorf("%w: 1-%d points per batch", ErrBadMessage, MaxPointsPerMsg)
	}
	if in.GetWidth() > maxWidth {
		return StrokeBatch{}, fmt.Errorf("%w: width must be %d-%d", ErrBadMessage, minWidth, maxWidth)
	}
	points := make([][]int, len(flat)/3)
	for i := range points {
		points[i] = []int{int(flat[3*i]), int(flat[3*i+1]), int(flat[3*i+2])}
	}
	m := StrokeBatch{StrokeID: in.GetStrokeId(), Points: points, Color: in.GetColor(), Width: int(in.GetWidth()), Done: in.GetDone()}
	return m, validateStroke(m)
}

// EncodeStrokeEvent is a stroke_batch event as a binary frame, for sockets that negotiated
// StrokesSubprotocol. The values were checked on the way in, so they fit the frame's types.
func EncodeStrokeEvent(seq int64, userID string, m StrokeBatch) []byte {
	flat := make([]uint32, 0, 3*len(m.Points))
	for _, p := range m.Points {
		flat = append(flat, uint32(p[0]), uint32(p[1]), uint32(p[2])) //nolint:gosec // validated: 0..600000
	}
	b, err := proto.Marshal(&gamev1.StrokeFrame{Frame: &gamev1.StrokeFrame_Event{Event: &gamev1.StrokeBatchEvent{
		Seq: seq, UserId: userID, StrokeId: m.StrokeID, Points: flat, Color: m.Color,
		Width: uint32(m.Width), Done: m.Done, //nolint:gosec // validated: 1..200
	}}})
	if err != nil {
		// Only server-built values are marshalled here; failing is a programming error.
		panic(fmt.Sprintf("marshal stroke event: %v", err))
	}
	return b
}
