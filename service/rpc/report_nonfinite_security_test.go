package rpc

import (
	"context"
	"math"
	"testing"

	"github.com/goccy/go-json"
	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// 回归：agent 上报 NaN/±Inf（protobuf double 合法值）会原样存进内存 State，
// 之后所有 JSON 序列化（/ws/server、/api/v1/server）都报 "unsupported value: NaN"，
// 任意持有 agent 密钥的机器即可让全站（含访客首页）数据推送失效，WS 协程还会空转占满 CPU。

type stateReportStream struct {
	ctx    context.Context
	states []*pb.State
}

func (s *stateReportStream) Send(*pb.Receipt) error { return nil }
func (s *stateReportStream) Recv() (*pb.State, error) {
	if len(s.states) == 0 {
		return nil, context.Canceled
	}
	st := s.states[0]
	s.states = s.states[1:]
	return st, nil
}
func (s *stateReportStream) SetHeader(metadata.MD) error  { return nil }
func (s *stateReportStream) SendHeader(metadata.MD) error { return nil }
func (s *stateReportStream) SetTrailer(metadata.MD)       {}
func (s *stateReportStream) Context() context.Context     { return s.ctx }
func (s *stateReportStream) SendMsg(any) error            { return nil }
func (s *stateReportStream) RecvMsg(any) error            { return context.Canceled }

func TestReportSystemStateRejectsNonFiniteFloats(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"member-secret": 200})

	authed := requestTaskSecurityAuthedStream("member-secret", reporter.UUID)
	stream := &stateReportStream{ctx: authed.ctx, states: []*pb.State{{
		Cpu: math.NaN(), Load1: math.Inf(1), Load5: math.Inf(-1), Load15: math.NaN(), MemUsed: 1,
	}}}
	_ = NewNezhaHandler().ReportSystemState(stream)

	server, _ := singleton.ServerShared.Get(reporter.ID)
	_, err := json.Marshal(server)
	t.Logf("上报 NaN/Inf 后 State=%+v, 序列化 err=%v", *server.State, err)
	if err != nil {
		t.Fatalf("非有限浮点不得进入运行态 State（序列化失败会拖垮 /ws/server 与 /api/v1/server）: %v", err)
	}
	if server.State.MemUsed != 1 {
		t.Fatalf("其余合法字段应保留, got %+v", *server.State)
	}
}
