package rpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/metadata"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// authCheckWithSecret drives (*authHandler).check end-to-end via the same
// gRPC metadata path the real RPC handler uses. Tests rely on it to assert
// what a real reconnect — secret + UUID supplied on the wire — would do.
func authCheckWithSecret(secret, uuid string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client_secret", secret,
		"client_uuid", uuid,
	))
	return (&authHandler{}).Check(ctx)
}

func authCheckWithHyphenatedSecret(secret, uuid string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client-secret", secret,
		"client-uuid", uuid,
	))
	return (&authHandler{}).Check(ctx)
}

// authCheckWithBothKeyStyles reproduces the real post-upgrade wire state: a
// new agent (PR #244) emits BOTH hyphenated and underscore metadata, so a new
// dashboard receives both at once. hyphenSecret/underscoreSecret may differ so
// a test can assert which key wins.
func authCheckWithBothKeyStyles(hyphenSecret, underscoreSecret, uuid string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client-secret", hyphenSecret,
		"client_secret", underscoreSecret,
		"client-uuid", uuid,
		"client_uuid", uuid,
	))
	return (&authHandler{}).Check(ctx)
}

// authHandshakeUUID is RFC4122-shaped so it survives the uuid.ParseUUID gate
// at the top of check().
const authHandshakeUUID = "11111111-1111-1111-1111-111111111111"

// setupAuthHandshakeFixture seeds a single server (id=11, owner=user 100,
// real UUID) plus the user-secret tables so the global-secret fall-through
// in check() has something to match.
func setupAuthHandshakeFixture(t *testing.T) func() {
	t.Helper()
	originalDB := singleton.DB
	originalServerShared := singleton.ServerShared
	originalUserInfoMap := singleton.UserInfoMap
	originalAgentSecretToUserId := singleton.AgentSecretToUserId

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.Server{}, &model.WAF{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&model.Server{
		Common: model.Common{ID: 11, UserID: 100},
		UUID:   authHandshakeUUID,
		Name:   "handshake-srv",
	}).Error; err != nil {
		t.Fatalf("create handshake server: %v", err)
	}
	singleton.DB = db
	singleton.ServerShared = singleton.NewServerClass()
	srv := &model.Server{Common: model.Common{ID: 11, UserID: 100}, UUID: authHandshakeUUID, Name: "handshake-srv"}
	model.InitServer(srv)
	singleton.ServerShared.Update(srv, authHandshakeUUID)

	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{
		100: {Role: model.RoleMember, AgentSecret: "alice-global"},
		200: {Role: model.RoleMember, AgentSecret: "bob-global"},
	}
	singleton.AgentSecretToUserId = map[string]uint64{
		"alice-global": 100,
		"bob-global":   200,
	}
	singleton.UserLock.Unlock()

	return func() {
		singleton.DB = originalDB
		singleton.ServerShared = originalServerShared
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfoMap
		singleton.AgentSecretToUserId = originalAgentSecretToUserId
		singleton.UserLock.Unlock()
	}
}

func TestAuthCheckAcceptsHyphenatedMetadata(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	cid, err := authCheckWithHyphenatedSecret("alice-global", authHandshakeUUID)
	if err != nil {
		t.Fatalf("hyphenated metadata must authenticate: %v", err)
	}
	if cid != 11 {
		t.Fatalf("expected server ID 11, got %d", cid)
	}
}

// The everyday post-upgrade case (new agent + new dashboard): both key styles
// arrive together carrying the same secret and must authenticate normally.
func TestAuthCheckAcceptsBothKeyStylesPresent(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	cid, err := authCheckWithBothKeyStyles("alice-global", "alice-global", authHandshakeUUID)
	if err != nil {
		t.Fatalf("an agent emitting both key styles must authenticate: %v", err)
	}
	if cid != 11 {
		t.Fatalf("expected server ID 11, got %d", cid)
	}
}

// When both styles are present the hyphenated key wins (firstMetadataValue
// lists it first). This pins the precedence so a future reorder can't silently
// start trusting the underscore alias that Caddy strips.
func TestAuthCheckHyphenatedKeyTakesPrecedence(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	cid, err := authCheckWithBothKeyStyles("alice-global", "garbage-underscore", authHandshakeUUID)
	if err != nil {
		t.Fatalf("hyphenated secret must be the one used, so auth must succeed: %v", err)
	}
	if cid != 11 {
		t.Fatalf("expected server ID 11 from the hyphenated secret, got %d", cid)
	}
}

// setupAuthAgentFixture seeds an in-memory DB and ServerShared with two
// servers belonging to different users so we can assert that a secret bound
// to user A cannot resolve a server UUID owned by user B.
func setupAuthAgentFixture(t *testing.T) func() {
	t.Helper()
	originalDB := singleton.DB
	originalServerShared := singleton.ServerShared

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.Server{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&model.Server{
		Common: model.Common{ID: 1, UserID: 100},
		UUID:   "uuid-alice",
		Name:   "alice-srv",
	}).Error; err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := db.Create(&model.Server{
		Common: model.Common{ID: 2, UserID: 200},
		UUID:   "uuid-bob",
		Name:   "bob-srv",
	}).Error; err != nil {
		t.Fatalf("create bob: %v", err)
	}
	singleton.DB = db
	singleton.ServerShared = singleton.NewServerClass()

	return func() {
		singleton.DB = originalDB
		singleton.ServerShared = originalServerShared
	}
}

func TestAuthorizeAgentForUUIDAcceptsOwnedServer(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	cid, hasID, err := authorizeAgentForUUID(100, "uuid-alice")
	if err != nil {
		t.Fatalf("alice with her own server UUID must not error, got %v", err)
	}
	if !hasID || cid != 1 {
		t.Fatalf("expected (cid=1, hasID=true), got (cid=%d, hasID=%v)", cid, hasID)
	}
}

// Core regression: an agent presenting user A's secret but user B's server
// UUID must be rejected. Previously the code returned the resolved server ID
// without verifying the UserID matched the secret owner, allowing same-tenant
// (and worse — cross-tenant if UUID leaks) server impersonation.
func TestAuthorizeAgentForUUIDRejectsForeignServerUUID(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	_, _, err := authorizeAgentForUUID(100, "uuid-bob") // alice's secret + bob's UUID
	if err == nil {
		t.Fatalf("UUID owned by another user must be rejected")
	}
}

func TestAuthorizeAgentForUUIDAllowsGlobalDefaultSecret(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	cid, hasID, err := authorizeAgentForUUID(0, "uuid-bob")
	if err != nil {
		t.Fatalf("global default secret must be allowed to use existing UUIDs, got %v", err)
	}
	if !hasID || cid != 2 {
		t.Fatalf("expected (cid=2, hasID=true), got (cid=%d, hasID=%v)", cid, hasID)
	}
}

// An unknown UUID must NOT be treated as an impersonation attempt — it is
// the normal first-time registration path and the caller (Check) creates a
// new server bound to the secret owner.
func TestAuthorizeAgentForUUIDPermitsUnknownUUIDForRegistration(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	cid, hasID, err := authorizeAgentForUUID(100, "uuid-never-seen-before")
	if err != nil {
		t.Fatalf("unknown UUID must be permitted for new registration, got %v", err)
	}
	if hasID {
		t.Fatalf("hasID must be false for unknown UUID, got cid=%d", cid)
	}
}

// wafAgentAuthFailCount returns the recorded WAF count for the given IP +
// gRPC block identifier. Used by the bad-credential WAF tests to assert
// FirstOrCreate / UPDATE actually fired.
func wafAgentAuthFailCount(t *testing.T, ip string) uint64 {
	t.Helper()
	bin, err := utils.IPStringToBinary(ip)
	if err != nil {
		t.Fatalf("ip parse: %v", err)
	}
	var w model.WAF
	res := singleton.DB.Where("ip = ? AND block_identifier = ?", bin, model.BlockIDgRPC).First(&w)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return 0
		}
		t.Fatalf("query waf: %v", res.Error)
	}
	return w.Count
}

// authCheckFromIP feeds an attacker IP through the real Check entry point
// so the WAF BlockIP path observes a non-empty CtxKeyRealIP.
func authCheckFromIP(secret, uuid, ip string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client_secret", secret,
		"client_uuid", uuid,
	))
	ctx = context.WithValue(ctx, model.CtxKeyRealIP{}, ip)
	return (&authHandler{}).Check(ctx)
}

// A bad secret paired with a malformed UUID short-circuits to "客户端 UUID 不合法"
// but must still increment the BlockIP(WAFBlockReasonTypeAgentAuthFail) counter
// — that counter is the only thing throttling brute-force on agent secrets.
func TestAuthBadSecretInvalidUUIDStillIncrementsAgentAuthFailWAF(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()
	const attackerIP = "203.0.113.7"

	if _, err := authCheckFromIP("definitely-not-a-real-secret", "not-a-uuid", attackerIP); err == nil {
		t.Fatal("Check must reject bogus credentials")
	}

	if got := wafAgentAuthFailCount(t, attackerIP); got == 0 {
		t.Fatalf("bad client_secret + invalid client_uuid must still count toward WAFBlockReasonTypeAgentAuthFail; got count=%d", got)
	}
}

// Mirror of the above for the empty-UUID metadata path. uuid.ParseUUID("")
// also errors out, so the same auth-fail counting must apply.
func TestAuthBadSecretEmptyUUIDStillIncrementsAgentAuthFailWAF(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()
	const attackerIP = "203.0.113.8"

	if _, err := authCheckFromIP("another-bad-secret", "", attackerIP); err == nil {
		t.Fatal("Check must reject bogus credentials")
	}

	if got := wafAgentAuthFailCount(t, attackerIP); got == 0 {
		t.Fatalf("bad client_secret + empty client_uuid must still count toward WAFBlockReasonTypeAgentAuthFail; got count=%d", got)
	}
}
