package rpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// dispatchTypeTaskStream 记录 DispatchTask 实际下发给 agent 的任务。
type dispatchTypeTaskStream struct{ sent []*proto.Task }

func (s *dispatchTypeTaskStream) Send(t *proto.Task) error         { s.sent = append(s.sent, t); return nil }
func (s *dispatchTypeTaskStream) Recv() (*proto.TaskResult, error) { return nil, context.Canceled }
func (s *dispatchTypeTaskStream) SetHeader(metadata.MD) error      { return nil }
func (s *dispatchTypeTaskStream) SendHeader(metadata.MD) error     { return nil }
func (s *dispatchTypeTaskStream) SetTrailer(metadata.MD)           {}
func (s *dispatchTypeTaskStream) Context() context.Context         { return context.Background() }
func (s *dispatchTypeTaskStream) SendMsg(any) error                { return nil }
func (s *dispatchTypeTaskStream) RecvMsg(any) error                { return context.Canceled }

func TestDispatchTaskSendsOnlyProbeTypes(t *testing.T) {
	originalServerShared := singleton.ServerShared
	originalUserInfo := singleton.UserInfoMap
	t.Cleanup(func() {
		singleton.ServerShared = originalServerShared
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfo
		singleton.UserLock.Unlock()
	})

	server := &model.Server{Common: model.Common{ID: 1, UserID: 100}}
	stream := &dispatchTypeTaskStream{}
	server.SetTaskStream(stream)
	serverShared := singleton.NewEmptyServerClassForTest()
	serverShared.InsertForTest(server)
	singleton.ServerShared = serverShared
	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{100: {Role: model.RoleMember}}
	singleton.UserLock.Unlock()

	bus := make(chan *model.Service, 8)
	done := make(chan struct{})
	go func() {
		DispatchTask(bus)
		close(done)
	}()
	for _, taskType := range []uint8{4, 13, 15, 255} {
		bus <- &model.Service{
			Common:      model.Common{ID: uint64(taskType), UserID: 100},
			Type:        taskType,
			Cover:       model.ServiceCoverIgnoreAll,
			SkipServers: map[uint64]bool{1: true},
		}
	}
	bus <- &model.Service{
		Common:      model.Common{ID: 1000, UserID: 100},
		Type:        model.TaskTypeTCPPing,
		Target:      "example.invalid:443",
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
	}
	close(bus)
	<-done

	require.Len(t, stream.sent, 1)
	require.Equal(t, uint64(model.TaskTypeTCPPing), stream.sent[0].GetType())
	require.Equal(t, uint64(1000), stream.sent[0].GetId())
}
