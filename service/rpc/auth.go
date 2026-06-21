package rpc

import (
	"context"
	"fmt"
	"strings"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/hashicorp/go-uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

type authHandler struct {
	ClientSecret string
	ClientUUID   string
}

func (a *authHandler) Check(ctx context.Context) (uint64, error) {
	return a.check(ctx)
}

func (a *authHandler) CheckRequestTask(ctx context.Context) (uint64, error) {
	return a.check(ctx)
}

func (a *authHandler) check(ctx context.Context) (uint64, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0, status.Errorf(codes.Unauthenticated, "获取 metaData 失败")
	}

	clientSecret := firstMetadataValue(md, "client-secret", "client_secret")

	if clientSecret == "" {
		return 0, status.Error(codes.Unauthenticated, "客户端认证失败")
	}

	ip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)

	clientUUID := firstMetadataValue(md, "client-uuid", "client_uuid")

	if _, err := uuid.ParseUUID(clientUUID); err != nil {
		// Keep this counter on the same trigger surface as the
		// unknown-secret path below: an attacker who pairs a bad secret
		// with a malformed/missing UUID otherwise bypasses
		// WAFBlockReasonTypeAgentAuthFail entirely and gets unbounded
		// retries (TestAuthBadSecret*InvalidUUIDStillIncrementsAgentAuthFailWAF).
		model.BlockIP(singleton.DB, ip, model.WAFBlockReasonTypeAgentAuthFail, model.BlockIDgRPC)
		return 0, status.Error(codes.Unauthenticated, "客户端 UUID 不合法")
	}

	singleton.UserLock.RLock()
	userId, ok := singleton.AgentSecretToUserId[clientSecret]
	if !ok {
		singleton.UserLock.RUnlock()
		model.BlockIP(singleton.DB, ip, model.WAFBlockReasonTypeAgentAuthFail, model.BlockIDgRPC)
		return 0, status.Error(codes.Unauthenticated, "客户端认证失败")
	}
	singleton.UserLock.RUnlock()

	model.UnblockIP(singleton.DB, ip, model.BlockIDgRPC)

	clientID, hasID, err := authorizeAgentForUUID(userId, clientUUID)
	if err != nil {
		return 0, status.Error(codes.Unauthenticated, err.Error())
	}
	if !hasID {
		s := model.Server{UUID: clientUUID, Name: petname.Generate(2, "-"), Common: model.Common{
			UserID: userId,
		}}
		if err := singleton.DB.Create(&s).Error; err != nil {
			return 0, status.Error(codes.Unauthenticated, err.Error())
		}

		model.InitServer(&s)
		singleton.ServerShared.Update(&s, clientUUID)

		clientID = s.ID
	}

	return clientID, nil
}

func firstMetadataValue(md metadata.MD, keys ...string) string {
	for _, key := range keys {
		if value, ok := md[key]; ok && len(value) > 0 {
			return strings.TrimSpace(value[0])
		}
	}
	return ""
}

// authorizeAgentForUUID resolves a client UUID to the dashboard's internal
// server ID, ensuring the resolved server is actually owned by the agent
// secret's owner. Previously Check returned the resolved server ID without
// verifying ownership, allowing an agent that knew another user's server
// UUID to impersonate it (poisoning monitoring state, triggering alerts).
// hasID=false means the UUID is unknown and the caller may register it as
// a new server for the secret owner.
//
// The error path also doubles as a leak-detection signal for operators: if
// an agent persistently fails with "client UUID does not belong to the
// agent secret owner", it pins down which user's secret has been reused
// against a server they don't own.
func authorizeAgentForUUID(userId uint64, clientUUID string) (clientID uint64, hasID bool, err error) {
	cid, found := singleton.ServerShared.UUIDToID(clientUUID)
	if !found {
		return 0, false, nil
	}
	server, _ := singleton.ServerShared.Get(cid)
	if server == nil {
		// Cache inconsistency: UUID maps to an ID, but no server record exists.
		// Treat as unknown (registration path) rather than impersonation.
		return 0, false, nil
	}
	if userId == 0 {
		// The legacy global agent secret maps to user 0. It predates per-user
		// agent secrets, so keep it compatible by allowing any existing UUID.
		return cid, true, nil
	}
	if server.GetUserID() == userId {
		return cid, true, nil
	}
	// server.UserID != userId — an impersonation attempt: an agent presenting
	// one user's secret for a server owned by another user.
	return 0, false, fmt.Errorf("client UUID does not belong to the agent secret owner")
}
