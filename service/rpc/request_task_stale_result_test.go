package rpc

import (
	"context"
	"errors"
	"testing"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// keepalive 结果不进 ServiceSentinel，便于在无 sentinel 的 fixture 中单测 stream 校验。
func staleTestResult() *pb.TaskResult { return &pb.TaskResult{Type: model.TaskTypeKeepalive} }

func TestRequestTaskRejectsResultWhenServerDeletedAfterRecv(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "10101010-1010-1010-1010-101010101010")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, map[uint64]model.UserInfo{200: {Role: model.RoleMember}}, map[string]uint64{"reporter-secret": 200})
	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	stream.results = []*pb.TaskResult{staleTestResult()}
	stream.onResult = func() { singleton.ServerShared.Delete([]uint64{reporter.ID}) }
	if err := NewNezhaHandler().RequestTask(stream); !errors.Is(err, ErrRequestTaskStreamSuperseded) {
		t.Fatalf("expected stale RequestTask stream error, got %v", err)
	}
}

func TestRequestTaskRejectsResultWhenNewerStreamSupersedesOld(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "20202020-2020-2020-2020-202020202020")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, map[uint64]model.UserInfo{200: {Role: model.RoleMember}}, map[string]uint64{"reporter-secret": 200})
	current, _ := singleton.ServerShared.Get(reporter.ID)
	newer := &requestTaskSecurityStream{ctx: context.Background()}
	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	stream.results = []*pb.TaskResult{staleTestResult()}
	stream.onResult = func() { current.SetTaskStream(newer) }
	if err := NewNezhaHandler().RequestTask(stream); !errors.Is(err, ErrRequestTaskStreamSuperseded) {
		t.Fatalf("expected superseded RequestTask stream error, got %v", err)
	}
	if got := current.GetTaskStream(); got != newer {
		t.Fatalf("old stream cleanup must preserve newer stream, got %T", got)
	}
}

func TestRequestTaskAcceptsResultAfterServerPointerReplacementWithSameStream(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "30303030-3030-3030-3030-303030303030")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, map[uint64]model.UserInfo{200: {Role: model.RoleMember}}, map[string]uint64{"reporter-secret": 200})
	old, _ := singleton.ServerShared.Get(reporter.ID)
	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	stream.results = []*pb.TaskResult{staleTestResult()}
	stream.onResult = func() {
		replacement := &model.Server{Common: model.Common{ID: old.ID, UserID: old.UserID}, UUID: old.UUID, Name: "replacement"}
		replacement.CopyFromRunningServer(old)
		singleton.ServerShared.Update(replacement, "")
	}
	if err := NewNezhaHandler().RequestTask(stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after accepted result, got %v", err)
	}
}
