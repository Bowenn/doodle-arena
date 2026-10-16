package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func stroke(points string) string {
	return `{"type":"stroke_batch","data":{"stroke_id":"s1","points":` + points + `,"color":"#1a2b3c","width":4,"done":false}}`
}

func TestDecodeClientAccepts(t *testing.T) {
	tests := []struct {
		raw  string
		want ClientMsg
	}{
		{`{"type":"hello","data":{"ticket":"abc","last_seq":7}}`, Hello{Ticket: "abc", LastSeq: 7}},
		{`{"type":"start"}`, Start{}},
		{`{"type":"start","data":{}}`, Start{}},
		{`{"type":"choose_word","data":{"index":2}}`, ChooseWord{Index: 2}},
		{`{"type":"undo","data":{"stroke_id":"a_b-1"}}`, Undo{StrokeID: "a_b-1"}},
		{`{"type":"guess","data":{"text":"  Zebra\u0007 "}}`, Guess{Text: "Zebra"}},
		{`{"type":"ping","data":{"t":123}}`, Ping{T: 123}},
	}
	for _, tt := range tests {
		got, err := DecodeClient([]byte(tt.raw))
		if err != nil {
			t.Errorf("%s: %v", tt.raw, err)
			continue
		}
		if g, _ := json.Marshal(got); string(g) != mustJSON(tt.want) {
			t.Errorf("%s: got %s, want %s", tt.raw, g, mustJSON(tt.want))
		}
	}
	m, err := DecodeClient([]byte(stroke(`[[0,0,0],[10000,10000,600000]]`)))
	if err != nil {
		t.Fatal(err)
	}
	if s := m.(StrokeBatch); len(s.Points) != 2 || s.Points[1][0] != 10000 {
		t.Fatalf("stroke = %+v", s)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestDecodeClientRejects(t *testing.T) {
	many := "[" + strings.Repeat("[1,1,1],", MaxPointsPerMsg) + "[1,1,1]]"
	tests := map[string]string{
		"not json":             `nope`,
		"unknown type":         `{"type":"teleport"}`,
		"unknown field":        `{"type":"guess","data":{"text":"a","admin":true}}`,
		"hello without ticket": `{"type":"hello","data":{}}`,
		"negative last_seq":    `{"type":"hello","data":{"ticket":"x","last_seq":-1}}`,
		"huge ticket":          `{"type":"hello","data":{"ticket":"` + strings.Repeat("a", 3000) + `"}}`,
		"negative index":       `{"type":"choose_word","data":{"index":-1}}`,
		"short point":          stroke(`[[1,2]]`),
		"long point":           stroke(`[[1,2,3,4]]`),
		"float coordinate":     stroke(`[[1.5,2,3]]`),
		"x too big":            stroke(`[[10001,2,3]]`),
		"negative y":           stroke(`[[1,-2,3]]`),
		"t too big":            stroke(`[[1,2,600001]]`),
		"no points":            stroke(`[]`),
		"513 points":           stroke(many),
		"bad color":            `{"type":"stroke_batch","data":{"stroke_id":"s1","points":[[1,1,1]],"color":"red","width":4}}`,
		"zero width":           `{"type":"stroke_batch","data":{"stroke_id":"s1","points":[[1,1,1]],"color":"#000000","width":0}}`,
		"bad stroke id":        `{"type":"undo","data":{"stroke_id":"../x"}}`,
		"empty guess":          `{"type":"guess","data":{"text":"   "}}`,
		"long guess":           `{"type":"guess","data":{"text":"` + strings.Repeat("a", 101) + `"}}`,
		"non-UTF-8 guess":      "{\"type\":\"guess\",\"data\":{\"text\":\"\xff\"}}",
	}
	for name, raw := range tests {
		if _, err := DecodeClient([]byte(raw)); !errors.Is(err, ErrBadMessage) {
			t.Errorf("%s: err = %v, want ErrBadMessage", name, err)
		}
	}
}

func TestMarshal(t *testing.T) {
	got := string(Marshal("event", Event{Seq: 3, Type: "chat", Data: map[string]string{"text": "hi"}}))
	want := `{"type":"event","data":{"seq":3,"type":"chat","data":{"text":"hi"}}}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
