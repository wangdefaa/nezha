package singleton

import (
	"fmt"
	"sync"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"gorm.io/gorm"
)

var (
	UserInfoMap         map[uint64]model.UserInfo
	AgentSecretToUserId map[string]uint64

	UserLock sync.RWMutex
)

func initUser() {
	UserInfoMap = make(map[uint64]model.UserInfo)
	AgentSecretToUserId = make(map[string]uint64)

	var users []model.User
	DB.Find(&users)

	// for backward compatibility
	UserInfoMap[0] = model.UserInfo{
		Role:        model.RoleAdmin,
		AgentSecret: Conf.AgentSecretKey,
	}
	AgentSecretToUserId[Conf.AgentSecretKey] = 0

	for _, u := range users {
		if u.AgentSecret == "" {
			u.AgentSecret = utils.MustGenerateRandomString(model.DefaultAgentSecretLength)
			if err := DB.Save(&u).Error; err != nil {
				panic(fmt.Errorf("update of user %d failed: %v", u.ID, err))
			}
		}

		UserInfoMap[u.ID] = model.UserInfo{
			Role:        u.Role,
			Username:    u.Username,
			AgentSecret: u.AgentSecret,
		}
		AgentSecretToUserId[u.AgentSecret] = u.ID
	}

	model.ServerOwnerLookup = lookupServerOwner
}

// lookupServerOwner resolves Server.UserID into a display-ready owner
// record for model.Server.MarshalJSON. uid=0 is the legacy global agent
// secret (a pseudo-owner with no User row) and intentionally returns
// ok=false with no username; the frontend renders it as "Global". Other
// uids return ok=false when the user has been deleted, so the JSON still
// carries the bare id and the frontend can render an "Unknown (#<uid>)"
// placeholder. RLock is required because OnUserUpdate / OnUserDelete may
// mutate UserInfoMap concurrently with serialization.
func lookupServerOwner(uid uint64) (model.ServerOwnerInfo, bool) {
	if uid == 0 {
		return model.ServerOwnerInfo{}, false
	}
	UserLock.RLock()
	info, ok := UserInfoMap[uid]
	UserLock.RUnlock()
	if !ok {
		return model.ServerOwnerInfo{}, false
	}
	return model.ServerOwnerInfo{ID: uid, Username: info.Username}, true
}

// userIsAdmin 供拨测等业务复用（servicesentinel.go / server.go）。
// 0 号用户是历史 global-secret 伪 owner：initUser 以 RoleAdmin 把它放进 UserInfoMap，
// 这里的显式判断让 initUser 之前的调用也按 admin 处理。
func userIsAdmin(userID uint64) bool {
	return userID == 0 || UserRole(userID).IsAdmin()
}

// UserRole 返回内存中的用户角色，查不到按普通成员处理（0 号见 initUser 的兼容条目）。
func UserRole(userID uint64) model.Role {
	UserLock.RLock()
	defer UserLock.RUnlock()
	if u, ok := UserInfoMap[userID]; ok {
		return u.Role
	}
	return model.RoleMember
}

func OnUserUpdate(u *model.User) {
	UserLock.Lock()
	defer UserLock.Unlock()

	if u == nil {
		return
	}

	UserInfoMap[u.ID] = model.UserInfo{
		Role:        u.Role,
		Username:    u.Username,
		AgentSecret: u.AgentSecret,
	}
	AgentSecretToUserId[u.AgentSecret] = u.ID
}

func OnUserDelete(id []uint64, errorFunc func(string, ...any) error) error {
	if len(id) < 1 {
		return Localizer.ErrorT("user id not specified")
	}
	deleted, changes, err := deleteUsersLocked(id, errorFunc)
	// 必须在释放 UserLock 之后再清内存：ServerShared.Delete 取 lifecycleMu 写锁，
	// 而拨测上报在 lifecycleMu 读锁内会经 userIsAdmin 取 UserLock 读锁，持 UserLock 调用会死锁；
	// 换上改写过引用的规则与拨测时要取的 AlertsLock、拨测锁同理。
	if len(deleted) > 0 {
		afterServersDeleted(deleted, changes)
	}
	return err
}

// deleteUsersLocked 在 UserLock 内逐个删除用户及其 server 并清理凭据映射，
// 返回已从库中删除的 server id，以及改写过引用的规则与拨测。
func deleteUsersLocked(id []uint64, errorFunc func(string, ...any) error) ([]uint64, refChanges, error) {
	UserLock.Lock()
	defer UserLock.Unlock()

	var deleted []uint64
	var changes refChanges
	slist := ServerShared.GetSortedList()
	for _, uid := range id {
		servers := model.FindByUserID(slist, uid)
		c, err := deleteUserTx(uid, servers)
		if err != nil {
			return deleted, changes, errorFunc("%v", err)
		}
		deleted = append(deleted, servers...)
		changes.add(c)
		secret := UserInfoMap[uid].AgentSecret
		delete(AgentSecretToUserId, secret)
		delete(UserInfoMap, uid)
	}
	return deleted, changes, nil
}

// deleteUserTx 在一个事务里删除用户和属于该用户的 server。
func deleteUserTx(uid uint64, servers []uint64) (refChanges, error) {
	var changes refChanges
	err := DB.Transaction(func(tx *gorm.DB) (err error) {
		if len(servers) > 0 {
			if changes, err = deleteServersTx(tx, servers); err != nil {
				return err
			}
		}
		return tx.Where("id = ?", uid).Delete(&model.User{}).Error
	})
	return changes, err
}

func dropAlertTransferStats(servers []uint64) {
	AlertsLock.Lock()
	defer AlertsLock.Unlock()
	for _, sid := range servers {
		for _, alert := range Alerts {
			if AlertsCycleTransferStatsStore[alert.ID] != nil {
				delete(AlertsCycleTransferStatsStore[alert.ID].ServerName, sid)
				delete(AlertsCycleTransferStatsStore[alert.ID].Transfer, sid)
				delete(AlertsCycleTransferStatsStore[alert.ID].NextUpdate, sid)
			}
		}
	}
}
