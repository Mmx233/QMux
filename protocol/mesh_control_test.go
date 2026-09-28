package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestMeshControlFrameBoundsAndStrictDecode(t *testing.T) {
	chunk := MeshChunk{Record: 0, Total: MaxMeshChunkDataSize, Digest: bytes.Repeat([]byte{1}, 32), Data: bytes.Repeat([]byte{2}, MaxMeshChunkDataSize)}
	frame, err := MarshalMeshControlFrame(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame)-5 > MaxControlPayloadSize {
		t.Fatalf("chunk payload = %d", len(frame)-5)
	}
	decoded, err := ReadMeshControl(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.(MeshChunk); !bytes.Equal(got.Data, chunk.Data) || !bytes.Equal(got.Digest, chunk.Digest) {
		t.Fatalf("chunk round trip = %+v", got)
	}
	chunk.Data = append(chunk.Data, 1)
	if _, err := MarshalMeshControlFrame(chunk); err == nil {
		t.Fatal("oversized raw chunk accepted")
	}
	chunk.Data = []byte{1}
	chunk.Offset = 1
	if _, err := MarshalMeshControlFrame(chunk); err == nil {
		t.Fatal("non-first chunk with digest accepted")
	}

	for _, payload := range []string{
		`{"Revision":0,"Groups":0,"Paths":0,"GroupBytes":0,"Unknown":true}`,
		`{"Revision":0,"Revision":1,"Groups":0,"Paths":0,"GroupBytes":0}`,
		`{"Revision":0,"Groups":0,"Paths":0,"GroupBytes":0}{}`,
	} {
		if _, err := DecodeMeshControl(MsgTypeMeshBegin, []byte(payload)); err == nil {
			t.Fatalf("invalid Begin accepted: %s", payload)
		}
	}
	header := make([]byte, 5)
	header[0] = MsgTypeMeshChunk
	binary.BigEndian.PutUint32(header[1:], MaxControlPayloadSize+1)
	if _, err := ReadMeshControl(bytes.NewReader(header)); err == nil || !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("oversized frame error = %v", err)
	}
}
