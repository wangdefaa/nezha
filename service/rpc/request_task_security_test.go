package rpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

type requestTaskSecurityStream struct {
	ctx     context.Context
	results []*pb.TaskResult
	onRecv  func()
	onSend  func(*pb.Task)
	sendErr error
}

func (s *requestTaskSecurityStream) Send(task *pb.Task) error {
	if s.onSend != nil {
		s.onSend(task)
	}
	return s.sendErr
}

func (s *requestTaskSecurityStream) Recv() (*pb.TaskResult, error) {
	if len(s.results) == 0 {
		if s.onRecv != nil {
			s.onRecv()
		}
		return nil, context.Canceled
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func (s *requestTaskSecurityStream) SetHeader(metadata.MD) error  { return nil }
func (s *requestTaskSecurityStream) SendHeader(metadata.MD) error { return nil }
func (s *requestTaskSecurityStream) SetTrailer(metadata.MD)       {}
func (s *requestTaskSecurityStream) Context() context.Context     { return s.ctx }
func (s *requestTaskSecurityStream) SendMsg(any) error            { return nil }
func (s *requestTaskSecurityStream) RecvMsg(any) error            { return context.Canceled }

func TestRequestTaskClearsTaskStreamOnRecvError(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "cccccccc-cccc-cccc-cccc-cccccccccccc")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	stream := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	err := NewNezhaHandler().RequestTask(stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after Recv error, got %v", err)
	}

	server, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found", reporter.ID)
	}
	if got := server.GetTaskStream(); got != nil {
		t.Fatalf("dead RequestTask stream must be cleared, got %T", got)
	}
}

func TestRequestTaskKeepsNewerTaskStreamOnOldRecvError(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "dddddddd-dddd-dddd-dddd-dddddddddddd")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})

	server, ok := singleton.ServerShared.Get(reporter.ID)
	if !ok {
		t.Fatalf("server %d not found", reporter.ID)
	}
	newer := &requestTaskSecurityStream{ctx: context.Background()}
	old := requestTaskSecurityAuthedStream("reporter-secret", reporter.UUID)
	old.onRecv = func() {
		server.SetTaskStream(newer)
	}

	err := NewNezhaHandler().RequestTask(old)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after Recv error, got %v", err)
	}
	if got := server.GetTaskStream(); got != newer {
		t.Fatalf("old stream cleanup must keep newer stream, got %T", got)
	}
}

func setupRequestTaskSecurityFixture(t *testing.T, servers []*model.Server, users map[uint64]model.UserInfo, agentSecrets map[string]uint64) {
	t.Helper()

	originalDB := singleton.DB
	originalConf := singleton.Conf
	originalLoc := singleton.Loc
	originalServerShared := singleton.ServerShared
	originalCronShared := singleton.CronShared
	originalUserInfoMap := singleton.UserInfoMap
	originalAgentSecretToUserID := singleton.AgentSecretToUserId

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)

	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{}}
	singleton.Loc = time.UTC
	if err := singleton.DB.AutoMigrate(model.Server{}); err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		if err := singleton.DB.Create(server).Error; err != nil {
			t.Fatal(err)
		}
	}

	singleton.UserLock.Lock()
	singleton.UserInfoMap = users
	singleton.AgentSecretToUserId = agentSecrets
	singleton.UserLock.Unlock()
	singleton.ServerShared = singleton.NewServerClass()
	singleton.CronShared = singleton.NewCronClass()

	t.Cleanup(func() {
		if singleton.CronShared != nil && singleton.CronShared.Cron != nil {
			singleton.CronShared.Stop()
		}
		sqlDB.Close()
		singleton.DB = originalDB
		singleton.Conf = originalConf
		singleton.Loc = originalLoc
		singleton.ServerShared = originalServerShared
		singleton.CronShared = originalCronShared
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfoMap
		singleton.AgentSecretToUserId = originalAgentSecretToUserID
		singleton.UserLock.Unlock()
	})
}

func requestTaskSecurityServer(id, userID uint64, uuid string) *model.Server {
	return &model.Server{
		Common: model.Common{ID: id, UserID: userID},
		UUID:   uuid,
		Name:   "request-task-security-server",
	}
}

func requestTaskSecurityAuthedStream(secret string, uuid string) *requestTaskSecurityStream {
	return &requestTaskSecurityStream{
		ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"client_secret", secret,
			"client_uuid", uuid,
		)),
	}
}
