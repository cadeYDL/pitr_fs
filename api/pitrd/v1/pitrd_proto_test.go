package v1

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestProto_MessageRoundTrip(t *testing.T) {
	want := &LogsResponse{
		Entries: []*LogEntry{{
			Transaction: &Transaction{
				TxnId:       42,
				VersionHash: "012345abcdef",
				ParentId:    proto.Int64(7),
				ScopePath:   "/workspace/proj",
				State:       "committed",
				Command:     "commit",
				Message:     "完成 parser 修复",
				CreatedAt:   timestamppb.New(time.Unix(1_700_000_000, 123)),
				ClosedAt:    timestamppb.New(time.Unix(1_700_000_100, 456)),
			},
		}},
	}

	data, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := new(LogsResponse)
	if err := proto.Unmarshal(data, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(want, got) {
		t.Fatalf("round trip 不保真:\nwant=%v\ngot =%v", want, got)
	}
}
