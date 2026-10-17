// Package protocol is the WebSocket wire format (design doc, section 6.4): JSON messages
// shaped {"type": ..., "data": ...}. It checks the structure of client messages; whether a
// message is allowed right now (phase, role) is the room's decision.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxMessageBytes is the largest message a client may send. Server messages, such as a
// welcome that carries the canvas, can be larger.
const MaxMessageBytes = 64 << 10

// Close codes the server uses (4000-4999 are application codes).
const (
	CloseBadRequest   = 4000
	CloseBadTicket    = 4001 // invalid, expired or already used
	CloseRoomFull     = 4003
	CloseNoRoom       = 4004
	CloseHelloTimeout = 4008
	CloseReplaced     = 4009 // the same user connected again
	// CloseRoomMoved: the room isn't (or is no longer) on this server; the client asks the
	// lobby where it is now and resumes there (ADR 0007).
	CloseRoomMoved = 4010
)

// Client message types.
const (
	TypeHello       = "hello"
	TypeStart       = "start"
	TypeChooseWord  = "choose_word"
	TypeStrokeBatch = "stroke_batch"
	TypeUndo        = "undo"
	TypeGuess       = "guess"
	TypePing        = "ping"
)

// Limits on client input.
const (
	MaxCoord        = 10000 // coordinates are normalized to 0..10000
	MaxPointsPerMsg = 512
	MaxStrokeTimeMS = 600_000
	MaxGuessRunes   = 100
	maxTicketBytes  = 2048
	minWidth        = 1
	maxWidth        = 200
)

// ErrBadMessage wraps every validation failure.
var ErrBadMessage = errors.New("bad message")

// ClientMsg is one decoded, structurally valid client message.
type ClientMsg interface{ clientMsg() }

// Hello is the first message on every connection.
type Hello struct {
	Ticket  string `json:"ticket"`
	LastSeq int64  `json:"last_seq"`
}

// Start asks to start a match (host only).
type Start struct{}

// ChooseWord picks one of the offered words (drawer only).
type ChooseWord struct {
	Index int `json:"index"`
}

// StrokeBatch is part of a stroke (drawer only). Points are [x, y, t_ms].
type StrokeBatch struct {
	StrokeID string  `json:"stroke_id"`
	Points   [][]int `json:"points"`
	Color    string  `json:"color"`
	Width    int     `json:"width"`
	Done     bool    `json:"done"`
}

// Undo removes one of the sender's own strokes.
type Undo struct {
	StrokeID string `json:"stroke_id"`
}

// Guess is a guess during a round, and chat otherwise.
type Guess struct {
	Text string `json:"text"`
}

// Ping measures round-trip time; the server answers pong with the same t.
type Ping struct {
	T int64 `json:"t"`
}

func (Hello) clientMsg()       {}
func (Start) clientMsg()       {}
func (ChooseWord) clientMsg()  {}
func (StrokeBatch) clientMsg() {}
func (Undo) clientMsg()        {}
func (Guess) clientMsg()       {}
func (Ping) clientMsg()        {}

type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

var (
	strokeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	colorPattern    = regexp.MustCompile(`^#[0-9a-f]{6}$`)
)

// DecodeClient parses and validates one client message.
func DecodeClient(raw []byte) (ClientMsg, error) {
	// encoding/json quietly turns invalid UTF-8 into U+FFFD, so check the raw bytes first.
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: messages must be UTF-8", ErrBadMessage)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: not a JSON envelope", ErrBadMessage)
	}
	data := env.Data
	if len(data) == 0 || string(data) == "null" {
		data = []byte("{}")
	}
	switch env.Type {
	case TypeHello:
		var m Hello
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		if m.Ticket == "" || len(m.Ticket) > maxTicketBytes || m.LastSeq < 0 {
			return nil, fmt.Errorf("%w: hello needs a ticket and last_seq >= 0", ErrBadMessage)
		}
		return m, nil
	case TypeStart:
		var m Start
		return m, strict(data, &m)
	case TypeChooseWord:
		var m ChooseWord
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		if m.Index < 0 {
			return nil, fmt.Errorf("%w: index must be >= 0", ErrBadMessage)
		}
		return m, nil
	case TypeStrokeBatch:
		var m StrokeBatch
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		return m, validateStroke(m)
	case TypeUndo:
		var m Undo
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		if !strokeIDPattern.MatchString(m.StrokeID) {
			return nil, fmt.Errorf("%w: bad stroke_id", ErrBadMessage)
		}
		return m, nil
	case TypeGuess:
		var m Guess
		if err := strict(data, &m); err != nil {
			return nil, err
		}
		text, err := CleanText(m.Text, MaxGuessRunes)
		if err != nil {
			return nil, err
		}
		return Guess{Text: text}, nil
	case TypePing:
		var m Ping
		return m, strict(data, &m)
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrBadMessage, env.Type)
	}
}

// strict decodes JSON and rejects unknown fields and trailing data.
func strict(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadMessage, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data", ErrBadMessage)
	}
	return nil
}

func validateStroke(m StrokeBatch) error {
	if !strokeIDPattern.MatchString(m.StrokeID) {
		return fmt.Errorf("%w: bad stroke_id", ErrBadMessage)
	}
	if !colorPattern.MatchString(m.Color) {
		return fmt.Errorf("%w: color must be #rrggbb", ErrBadMessage)
	}
	if m.Width < minWidth || m.Width > maxWidth {
		return fmt.Errorf("%w: width must be %d-%d", ErrBadMessage, minWidth, maxWidth)
	}
	if len(m.Points) == 0 || len(m.Points) > MaxPointsPerMsg {
		return fmt.Errorf("%w: 1-%d points per batch", ErrBadMessage, MaxPointsPerMsg)
	}
	// Points decode as []int, not [3]int: encoding/json fills a fixed-size array with zeros,
	// so [1,2] would silently become [1,2,0].
	for _, p := range m.Points {
		if len(p) != 3 {
			return fmt.Errorf("%w: a point is [x, y, t_ms]", ErrBadMessage)
		}
		if p[0] < 0 || p[0] > MaxCoord || p[1] < 0 || p[1] > MaxCoord {
			return fmt.Errorf("%w: coordinates are 0-%d", ErrBadMessage, MaxCoord)
		}
		if p[2] < 0 || p[2] > MaxStrokeTimeMS {
			return fmt.Errorf("%w: t_ms is 0-%d", ErrBadMessage, MaxStrokeTimeMS)
		}
	}
	return nil
}

// CleanText requires valid UTF-8, drops control characters, trims spaces and checks that
// 1..maxRunes runes are left.
func CleanText(s string, maxRunes int) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: text must be UTF-8", ErrBadMessage)
	}
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if n := utf8.RuneCountInString(s); n == 0 || n > maxRunes {
		return "", fmt.Errorf("%w: text must be 1-%d characters", ErrBadMessage, maxRunes)
	}
	return s, nil
}

// Marshal builds a server message {"type": typ, "data": data}.
func Marshal(typ string, data any) []byte {
	b, err := json.Marshal(struct {
		Type string `json:"type"`
		Data any    `json:"data"`
	}{typ, data})
	if err != nil {
		// Only server-built values are marshalled here; failing is a programming error.
		panic(fmt.Sprintf("marshal %s: %v", typ, err))
	}
	return b
}

// Event is a shared room event: it has a seq and every player receives it.
type Event struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
	Data any    `json:"data"`
}

// ErrorData is the data of an "error" message.
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
