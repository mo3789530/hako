package workspaceprotocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestDataFrameRoundTrip(t *testing.T) {
	want := []byte("echo hako\n")
	encoded, err := EncodeData(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(encoded)
	if err != nil || got.Resize || !bytes.Equal(got.Data, want) {
		t.Fatalf("Decode() = %#v, %v", got, err)
	}
}

func TestResizeFrameRoundTrip(t *testing.T) {
	encoded, err := EncodeResize(120, 40)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(encoded)
	if err != nil || !got.Resize || got.Columns != 120 || got.Rows != 40 {
		t.Fatalf("Decode() = %#v, %v", got, err)
	}
}

func TestDecoderHandlesFragmentedAndCoalescedStreamWrites(t *testing.T) {
	first, _ := EncodeData([]byte("one"))
	second, _ := EncodeResize(100, 30)
	stream := append(append([]byte(nil), first...), second...)
	var decoder Decoder
	frames, err := decoder.Feed(stream[:4])
	if err != nil || len(frames) != 0 || decoder.Pending() != 4 {
		t.Fatalf("first partial Feed() = %#v, %v pending=%d", frames, err, decoder.Pending())
	}
	frames, err = decoder.Feed(stream[4:])
	if err != nil || len(frames) != 2 || string(frames[0].Data) != "one" || !frames[1].Resize || frames[1].Columns != 100 || frames[1].Rows != 30 || decoder.Pending() != 0 {
		t.Fatalf("coalesced Feed() = %#v, %v pending=%d", frames, err, decoder.Pending())
	}
}

func TestDecoderBoundsFrameBufferWhileAcceptingLargeGatewayChunks(t *testing.T) {
	data := bytes.Repeat([]byte{'x'}, MaxFrame-HeaderSize)
	frame, err := EncodeData(data)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat(frame, MaxChunk/len(frame))
	var decoder Decoder
	frames, err := decoder.Feed(chunk)
	if err != nil || len(frames) != MaxChunk/len(frame) || decoder.Pending() != 0 {
		t.Fatalf("Feed(%d-byte Gateway chunk) frames=%d pending=%d err=%v", len(chunk), len(frames), decoder.Pending(), err)
	}
	if _, err := decoder.Feed(make([]byte, MaxChunk+1)); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("oversized chunk error = %v", err)
	}
}

func TestRejectsMalformedAndOversizedFrames(t *testing.T) {
	for _, frame := range [][]byte{
		{}, []byte("NOPE\x01x"), []byte("HAKS\x01"),
		append([]byte("HAKS\x02"), 0, 0, 0, 24),
		append([]byte("HAKS\x01"), bytes.Repeat([]byte{'x'}, MaxFrame)...),
	} {
		if _, err := Decode(frame); !errors.Is(err, ErrInvalidFrame) {
			t.Errorf("Decode(%d-byte frame) error = %v", len(frame), err)
		}
	}
	if _, err := EncodeData(nil); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("empty data error = %v", err)
	}
	if _, err := EncodeResize(501, 40); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("oversized terminal error = %v", err)
	}
}
