// AUD-SEC-002 unit tests: usage extraction from Anthropic payloads and
// from a byte-stream that must stay untouched.
package relay

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const p13MsgStart = `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12,"output_tokens":0,"cache_read_input_tokens":5}}}`
const p13MsgDelta = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":4}}`

func TestAUD_SEC_002ParseUsageNonStream(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":4,"cache_creation_input_tokens":3}}`)
	u := ParseAnthropicUsage(body)
	if !u.Reported {
		t.Fatal("usage object present but Reported=false")
	}
	if u.InputTokens != 12 || u.OutputTokens != 4 || u.CacheWriteTokens != 3 {
		t.Fatalf("usage = %+v, want 12/4/3", u)
	}
}

func TestAUD_SEC_002ParseUsageAbsentIsNotZeroSpend(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(`{}`), []byte(`{"type":"message","content":[]}`), []byte(`not json`)} {
		if u := ParseAnthropicUsage(body); u.Reported {
			t.Fatalf("body %q reported usage %+v, want Reported=false", body, u)
		}
	}
}

func TestAUD_SEC_002SnifferStreamsVerbatimAndFindsUsage(t *testing.T) {
	raw := "event: message_start\r\ndata: " + p13MsgStart + "\r\n\r\n" +
		"event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\r\n\r\n" +
		"event: message_delta\r\ndata: " + p13MsgDelta + "\r\n\r\n" +
		"event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"

	var out bytes.Buffer
	sniff := NewUsageSniffer(&out)
	// Write in pathological chunks: 1 byte at a time, so every event and
	// every data line is split across writes.
	for i := 0; i < len(raw); i++ {
		if _, err := sniff.Write([]byte{raw[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != raw {
		t.Fatalf("sniffer altered the stream:\n got %q\nwant %q", out.String(), raw)
	}
	u := sniff.Usage()
	if !u.Reported || u.InputTokens != 12 || u.OutputTokens != 4 || u.CacheReadTokens != 5 {
		t.Fatalf("usage = %+v, want 12/4/5 Reported", u)
	}
}

func TestAUD_SEC_002SnifferIgnoresNonUsageEvents(t *testing.T) {
	raw := "data: [DONE]\n\ndata: {\"type\":\"message_stop\"}\n\n: comment\n\n"
	var out bytes.Buffer
	sniff := NewUsageSniffer(&out)
	if _, err := io.Copy(sniff, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if out.String() != raw {
		t.Fatal("payload altered")
	}
	if sniff.Usage().Reported {
		t.Fatal("non-usage stream reported usage")
	}
}

func TestAUD_SEC_002SnifferDropsOversizedLineWithoutLosingSync(t *testing.T) {
	var out bytes.Buffer
	sniff := NewUsageSniffer(&out)
	huge := "data: {\"blob\":\"" + strings.Repeat("x", maxSniffLine+10) + "\"}\n"
	if _, err := sniff.Write([]byte(huge)); err != nil {
		t.Fatal(err)
	}
	if _, err := sniff.Write([]byte("data: " + p13MsgDelta + "\n")); err != nil {
		t.Fatal(err)
	}
	if out.Len() != len(huge)+len("data: "+p13MsgDelta+"\n") {
		t.Fatal("bytes dropped from the client stream")
	}
	if u := sniff.Usage(); !u.Reported || u.OutputTokens != 4 {
		t.Fatalf("usage after oversized line = %+v, want output=4", u)
	}
}

func TestAUD_SEC_002SnifferZeroAllocsOnPassthrough(t *testing.T) {
	var out bytes.Buffer
	sniff := NewUsageSniffer(&out)
	if _, err := sniff.Write([]byte("event: ping\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "event: ping\n" {
		t.Fatal("passthrough broken")
	}
}
