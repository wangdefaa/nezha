package model

import (
	"cmp"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	CtxKeyAuthorizedUser = "ckau"
	CtxKeyRealIPStr      = "ckri"
	CtxKeyIsIPMismatch   = "ckipm"
	CtxKeyAPIToken       = "ckpat"
)

type APITokenAccessor interface {
	CanAccessServer(uint64) bool
}

// PATFromContext 取请求上下文里的 PAT 访问器；JWT/匿名请求返回 nil。
func PATFromContext(ctx *gin.Context) APITokenAccessor {
	v, _ := ctx.Get(CtxKeyAPIToken)
	tok, _ := v.(APITokenAccessor)
	return tok
}

const (
	CacheKeyOauth2State = "cko2s::"
)

type CtxKeyRealIP struct{}
type CtxKeyConnectingIP struct{}

type Common struct {
	ID        uint64    `gorm:"primaryKey" json:"id,omitempty"`
	CreatedAt time.Time `gorm:"index;<-:create" json:"created_at,omitempty"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at,omitempty"`

	UserID uint64 `gorm:"index;default:0" json:"-"`
}

func (c *Common) GetID() uint64 {
	return c.ID
}

// GetUserID 原子读取所属用户 ID。改写 UserID 的服务器转移功能已移除，生产路径
// 目前没有并发写；保留 atomic 读写协议是为了让 SetUserID（仅测试使用）与
// 热路径读取不构成 data race（见 TestServerUserIDConcurrentAccessIsRaceFree）。
func (c *Common) GetUserID() uint64 {
	return atomic.LoadUint64(&c.UserID)
}

// SetUserID 原子改写所属用户 ID（生产代码已无调用方，保留给并发测试）。
// 普通 GORM AfterFind / Create 没有并发读，可以直接赋值。
func (c *Common) SetUserID(uid uint64) {
	atomic.StoreUint64(&c.UserID, uid)
}

func (c *Common) HasPermission(ctx *gin.Context) bool {
	auth, ok := ctx.Get(CtxKeyAuthorizedUser)
	if !ok {
		return false
	}

	user := *auth.(*User)
	if user.Role == RoleAdmin {
		return true
	}

	// 走 GetUserID 而不是裸读 c.UserID，与 SetUserID 的 atomic 协议保持一致
	// （TestCommonHasPermissionConcurrentWithSetUserIDIsRaceFree 在 -race 下钉死该不变量）。
	return user.ID == c.GetUserID()
}

type CommonInterface interface {
	GetID() uint64
	GetUserID() uint64
	HasPermission(*gin.Context) bool
}

func FindByUserID[S ~[]E, E CommonInterface](s S, uid uint64) []uint64 {
	var list []uint64
	for _, v := range s {
		if v.GetUserID() == uid {
			list = append(list, v.GetID())
		}
	}

	return list
}

func SearchByIDCtx[S ~[]E, E CommonInterface](c *gin.Context, x S) S {
	return SearchByID(strings.SplitSeq(c.Query("id"), ","), x)
}

func SearchByID[S ~[]E, E CommonInterface](seq iter.Seq[string], x S) S {
	if hasPriorityList[E]() {
		return searchByIDPri(seq, x)
	}

	var s S
	for idStr := range seq {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}

		s = appendBinarySearch(s, x, id)
	}
	return utils.IfOr(len(s) > 0, s, x)
}

func hasPriorityList[T CommonInterface]() bool {
	var class T

	switch any(class).(type) {
	case *Server:
		return true
	default:
		return false
	}
}

type splitter[S ~[]E, E CommonInterface] interface {
	// SplitList should split a sorted list into two separate lists:
	// The first list contains elements with a priority set (DisplayIndex != 0).
	// The second list contains elements without a priority set (DisplayIndex == 0).
	// The original slice is not modified. If no element without a priority is found, it returns nil.
	// Should be safe to use with a nil pointer.
	SplitList(x S) (S, S)
}

func searchByIDPri[S ~[]E, E CommonInterface](seq iter.Seq[string], x S) S {
	var class E
	split, ok := any(class).(splitter[S, E])
	if !ok {
		return x
	}

	plist, list2 := split.SplitList(x)

	var clist1, clist2 S
	for idStr := range seq {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}

		clist1 = appendSearch(clist1, plist, id)
		clist2 = appendBinarySearch(clist2, list2, id)
	}

	l := slices.Concat(clist1, clist2)
	return utils.IfOr(len(l) > 0, l, x)
}

func appendBinarySearch[S ~[]E, E CommonInterface](x, y S, target uint64) S {
	if i, ok := slices.BinarySearchFunc(y, target, func(e E, t uint64) int {
		return cmp.Compare(e.GetID(), t)
	}); ok {
		x = append(x, y[i])
	}
	return x
}

func appendSearch[S ~[]E, E CommonInterface](x, y S, target uint64) S {
	if i := slices.IndexFunc(y, func(e E) bool {
		return e.GetID() == target
	}); i != -1 {
		x = append(x, y[i])
	}

	return x
}
