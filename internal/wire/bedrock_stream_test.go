package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// readEventStreamBufferedReference is a test-only copy of the
// pre-215 readEventStream: it did io.ReadAll first and parsed the
// whole body afterwards, so no event was delivered until the body
// completed. It is the oracle for the equivalence test below — the
// incremental reader must produce byte-for-byte identical event
// sequences and identical errors on the same recorded streams.
func readEventStreamBufferedReference(r io.Reader, fn func(bedrockStreamEvent) error) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("wire: error reading Bedrock event stream: %w", err)
	}
	pos := 0
	for pos < len(data) {
		if len(data)-pos < 12 {
			return errors.New("wire: truncated Bedrock event-stream frame (prelude)")
		}
		totalLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		headersLen := int(binary.BigEndian.Uint32(data[pos+4 : pos+8]))
		preludeCRC := binary.BigEndian.Uint32(data[pos+8 : pos+12])
		if totalLen < 16 || pos+totalLen > len(data) {
			return errors.New("wire: truncated Bedrock event-stream frame (message)")
		}
		if crc32.ChecksumIEEE(data[pos:pos+8]) != preludeCRC {
			return errors.New("wire: Bedrock event-stream prelude CRC mismatch")
		}
		msgEnd := pos + totalLen
		if crc32.ChecksumIEEE(data[pos:msgEnd-4]) != binary.BigEndian.Uint32(data[msgEnd-4:msgEnd]) {
			return errors.New("wire: Bedrock event-stream message CRC mismatch")
		}
		headers, err := parseEventStreamHeaders(data[pos+12 : pos+12+headersLen])
		if err != nil {
			return err
		}
		payload := data[pos+12+headersLen : msgEnd-4]
		evType := headers[":event-type"]
		msgType := headers[":message-type"]
		if msgType == "exception" {
			exType := headers[":exception-type"]
			var decoded struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(payload, &decoded)
			if decoded.Message == "" {
				decoded.Message = headers[":message"]
			}
			return MapBedrockStreamError(exType, decoded.Message)
		}
		if evType == "" {
			return errors.New("wire: Bedrock event-stream frame has no :event-type header")
		}
		if err := fn(bedrockStreamEvent{Type: evType, Payload: bytes.Clone(payload)}); err != nil {
			return err
		}
		pos = msgEnd
	}
	return nil
}

// recordedBedrockStream is one recorded ConverseStream body used by
// the equivalence test: a text turn, a tool-use turn, and a
// reasoning turn, concatenated as separate streams below.
func recordedBedrockStreams(t *testing.T) [][]byte {
	t.Helper()
	text := concatFrames(t,
		"messageStart", map[string]any{"role": "assistant"},
		"contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}},
		"contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "hel"}},
		"contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "lo"}},
		"contentBlockStop", map[string]any{"contentBlockIndex": 0},
		"metadata", map[string]any{"usage": map[string]any{"inputTokens": 5, "outputTokens": 2, "totalTokens": 7}},
		"messageStop", map[string]any{"stopReason": "end_turn"},
	)
	tool := concatFrames(t,
		"messageStart", map[string]any{"role": "assistant"},
		"contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{"toolUse": map[string]any{"toolUseId": "t9", "name": "get_weather"}}},
		"contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"toolUse": map[string]any{"input": `{"city":`}}},
		"contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"toolUse": map[string]any{"input": `"Pune"}`}}},
		"contentBlockStop", map[string]any{"contentBlockIndex": 0},
		"contentBlockStart", map[string]any{"contentBlockIndex": 1, "start": map[string]any{}},
		"contentBlockDelta", map[string]any{"contentBlockIndex": 1, "delta": map[string]any{"reasoningContent": map[string]any{"text": "thinking…", "signature": "sig-1"}}},
		"contentBlockStop", map[string]any{"contentBlockIndex": 1},
		"messageStop", map[string]any{"stopReason": "tool_use"},
	)
	return [][]byte{text, tool}
}

// concatFrames builds a stream from alternating event-type / payload
// pairs.
func concatFrames(t *testing.T, pairs ...any) []byte {
	t.Helper()
	var out []byte
	for i := 0; i < len(pairs); i += 2 {
		evType, ok := pairs[i].(string)
		if !ok {
			t.Fatalf("concatFrames: pair %d event type is %T", i/2, pairs[i])
		}
		out = append(out, buildEventStreamFrame(t, evType, pairs[i+1])...)
	}
	return out
}

func collectEvents(r io.Reader, parse func(io.Reader, func(bedrockStreamEvent) error) error) ([]bedrockStreamEvent, error) {
	var events []bedrockStreamEvent
	err := parse(r, func(ev bedrockStreamEvent) error {
		events = append(events, bedrockStreamEvent{Type: ev.Type, Payload: bytes.Clone(ev.Payload)})
		return nil
	})
	return events, err
}

func eventsEqual(a, b []bedrockStreamEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || !bytes.Equal(a[i].Payload, b[i].Payload) {
			return false
		}
	}
	return true
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestReadEventStreamIdenticalToBufferedReader proves the
// incremental reader preserves the exact frame semantics of the old
// ReadAll reader (CHECKLIST 215): byte-for-byte identical event
// sequences (type + payload) and identical errors on recorded
// streams — a text turn, a tool-use + reasoning turn, an empty
// stream, a truncated prelude, a truncated message, a bad prelude
// CRC, a bad message CRC, a mid-stream exception, and a frame with
// no :event-type.
func TestReadEventStreamIdenticalToBufferedReader(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{name: "text", body: recordedBedrockStreams(t)[0]},
		{name: "tool+reasoning", body: recordedBedrockStreams(t)[1]},
		{name: "empty", body: nil},
		{name: "truncated-prelude", body: recordedBedrockStreams(t)[0][:20]},
		{name: "truncated-message", body: func() []byte {
			full := recordedBedrockStreams(t)[0]
			// First frame complete, second frame cut mid-payload.
			first := buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"})
			return append(append([]byte{}, first...), full[len(first):len(first)+10]...)
		}()},
		{name: "bad-prelude-crc", body: func() []byte {
			b := append([]byte{}, recordedBedrockStreams(t)[0]...)
			b[8] ^= 0xFF
			return b
		}()},
		{name: "bad-message-crc", body: func() []byte {
			b := append([]byte{}, recordedBedrockStreams(t)[0]...)
			b[len(b)-1] ^= 0xFF
			return b
		}()},
		{name: "exception-mid-stream", body: func() []byte {
			var out []byte
			out = append(out, buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"})...)
			out = append(out, buildEventStreamException(t, "com.amazonaws.bedrock#ThrottlingException", "slow down")...)
			return out
		}()},
		{name: "no-event-type", body: func() []byte {
			// A frame with only a :message-type header.
			var headers bytes.Buffer
			writeHeader := func(name, value string) {
				headers.WriteByte(byte(len(name)))
				headers.WriteString(name)
				headers.WriteByte(7)
				_ = binary.Write(&headers, binary.BigEndian, uint16(len(value)))
				headers.WriteString(value)
			}
			writeHeader(":message-type", "event")
			body := []byte(`{}`)
			totalLen := 12 + headers.Len() + len(body) + 4
			var frame bytes.Buffer
			_ = binary.Write(&frame, binary.BigEndian, uint32(totalLen))
			_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
			prelude := frame.Bytes()
			_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(prelude))
			frame.Write(headers.Bytes())
			frame.Write(body)
			_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
			return frame.Bytes()
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantEvents, wantErr := collectEvents(bytes.NewReader(tc.body), readEventStreamBufferedReference)
			gotEvents, gotErr := collectEvents(bytes.NewReader(tc.body), readEventStream)
			if !eventsEqual(wantEvents, gotEvents) {
				t.Fatalf("event sequences differ:\nold: %+v\nnew: %+v", wantEvents, gotEvents)
			}
			if errString(wantErr) != errString(gotErr) {
				t.Fatalf("errors differ: old %q, new %q", errString(wantErr), errString(gotErr))
			}
		})
	}
}

// trickleReader returns its chunks one at a time, sleeping delay
// before each chunk after the first, then EOF. It honours short
// reads: a chunk split across Read calls resumes where it stopped,
// so a small caller buffer (e.g. io.ReadAll's 512-byte starter)
// can never lose bytes.
type trickleReader struct {
	chunks [][]byte
	delay  time.Duration
	i      int
	off    int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	if r.off == 0 && r.i > 0 {
		time.Sleep(r.delay)
	}
	n := copy(p, r.chunks[r.i][r.off:])
	r.off += n
	if r.off >= len(r.chunks[r.i]) {
		r.i++
		r.off = 0
	}
	return n, nil
}

// TestReadEventStreamIncrementalDelivery is the timing proof for
// CHECKLIST 215: frames arrive 400 ms apart, and the first event
// must be delivered long before the body completes — the old
// ReadAll reader delivered nothing until the last byte arrived.
func TestReadEventStreamIncrementalDelivery(t *testing.T) {
	frames := [][]byte{
		buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}),
		buildEventStreamFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}),
		buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "first"}}),
		buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "second"}}),
		buildEventStreamFrame(t, "messageStop", map[string]any{"stopReason": "end_turn"}),
	}
	start := time.Now()
	var firstAt time.Time
	var mu sync.Mutex
	var seen []string
	done := make(chan error, 1)
	go func() {
		done <- readEventStream(&trickleReader{chunks: frames, delay: 400 * time.Millisecond}, func(ev bedrockStreamEvent) error {
			mu.Lock()
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			seen = append(seen, ev.Type)
			mu.Unlock()
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readEventStream: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readEventStream did not finish")
	}
	elapsed := time.Since(start)
	mu.Lock()
	defer mu.Unlock()
	if firstAt.IsZero() {
		t.Fatal("no event was delivered")
	}
	// 5 frames x 400 ms gaps = ~1.6 s total; the first frame is
	// available immediately, so it must arrive at least 500 ms
	// before the stream ends.
	if firstAt.Sub(start) > elapsed-500*time.Millisecond {
		t.Fatalf("first event arrived at %s into a %s stream, want >= 500 ms before the end (incremental delivery)", firstAt.Sub(start), elapsed)
	}
	if len(seen) != 5 {
		t.Fatalf("delivered %d events, want 5: %v", len(seen), seen)
	}
}

// TestReadEventStreamNeverEndingStreamDelivers proves the fixed
// limitation from CHECKLIST 205: a stream that never ends still
// delivers the frames it has already sent. The writer sends two
// frames, then blocks forever; both must arrive while the read is
// still open.
func TestReadEventStreamNeverEndingStreamDelivers(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}))
		_, _ = pw.Write(buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "still-going"}}))
		// Never write again, never close: the stream is endless.
		select {}
	}()
	defer pw.Close()
	got := make(chan bedrockStreamEvent, 4)
	errCh := make(chan error, 1)
	go func() {
		errCh <- readEventStream(pr, func(ev bedrockStreamEvent) error {
			got <- ev
			return nil
		})
	}()
	timeout := time.After(5 * time.Second)
	var events []bedrockStreamEvent
	for len(events) < 2 {
		select {
		case ev := <-got:
			events = append(events, ev)
		case err := <-errCh:
			t.Fatalf("readEventStream returned early with %v, want it to stay open on a never-ending stream", err)
		case <-timeout:
			t.Fatalf("only %d events arrived from a never-ending stream in 5 s, want 2", len(events))
		}
	}
	if events[0].Type != "messageStart" || events[1].Type != "contentBlockDelta" {
		t.Fatalf("events = %v, want [messageStart contentBlockDelta]", events)
	}
	if !strings.Contains(string(events[1].Payload), "still-going") {
		t.Fatalf("second payload = %s, want it to contain \"still-going\"", events[1].Payload)
	}
}

// oneByteReader delivers the stream one byte per Read call, proving
// frames split across arbitrary TCP segment boundaries still parse.
type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func TestReadEventStreamSplitAcrossTinyReads(t *testing.T) {
	want, _ := collectEvents(bytes.NewReader(recordedBedrockStreams(t)[1]), readEventStreamBufferedReference)
	got, err := collectEvents(&oneByteReader{data: recordedBedrockStreams(t)[1]}, readEventStream)
	if err != nil {
		t.Fatalf("readEventStream over 1-byte reads: %v", err)
	}
	if !eventsEqual(want, got) {
		t.Fatalf("event sequences differ over 1-byte reads:\nold: %+v\nnew: %+v", want, got)
	}
}

// TestTranslateBedrockStreamIncremental proves the chunk-level
// consequence: the first content chunk is emitted before the body
// completes, not after it.
func TestTranslateBedrockStreamIncremental(t *testing.T) {
	frames := [][]byte{
		buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}),
		buildEventStreamFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}),
		buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "hel"}}),
		buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "lo"}}),
		buildEventStreamFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0}),
		buildEventStreamFrame(t, "metadata", map[string]any{"usage": map[string]any{"inputTokens": 5, "outputTokens": 2, "totalTokens": 7}}),
		buildEventStreamFrame(t, "messageStop", map[string]any{"stopReason": "end_turn"}),
	}
	start := time.Now()
	var firstContentAt time.Time
	var mu sync.Mutex
	done := make(chan error, 1)
	var res StreamResult
	go func() {
		var err error
		res, err = TranslateBedrockStream(&trickleReader{chunks: frames, delay: 400 * time.Millisecond}, func(c ChatChunk) error {
			if len(c.Choices) > 0 && c.Choices[0].Delta.Content != "" {
				mu.Lock()
				if firstContentAt.IsZero() {
					firstContentAt = time.Now()
				}
				mu.Unlock()
			}
			return nil
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TranslateBedrockStream: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("TranslateBedrockStream did not finish")
	}
	elapsed := time.Since(start)
	if res.Content != "hello" {
		t.Fatalf("reassembled content = %q, want \"hello\"", res.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if firstContentAt.IsZero() {
		t.Fatal("no content chunk was emitted")
	}
	// 7 frames x 400 ms gaps = ~2.4 s; the first content frame is
	// available immediately.
	if firstContentAt.Sub(start) > elapsed-500*time.Millisecond {
		t.Fatalf("first content chunk arrived at %s into a %s stream, want >= 500 ms before the end", firstContentAt.Sub(start), elapsed)
	}
}
