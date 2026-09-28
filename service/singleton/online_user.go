package singleton

import (
	"sync"

	"github.com/nezhahq/nezha/model"
)

var (
	OnlineUserMap     = make(map[string]*model.OnlineUser)
	OnlineUserMapLock sync.Mutex
)

func AddOnlineUser(connId string, user *model.OnlineUser) {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()
	OnlineUserMap[connId] = user
}

func RemoveOnlineUser(connId string) {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()
	delete(OnlineUserMap, connId)
}

func BlockByIPs(ipList []string) error {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()

	for _, ip := range ipList {
		if err := model.BlockIP(DB, ip, model.WAFBlockReasonTypeManual, model.BlockIDManual); err != nil {
			return err
		}
		for _, user := range OnlineUserMap {
			if user.IP == ip && user.Conn != nil {
				user.Conn.Close()
			}
		}
	}

	return nil
}

func GetOnlineUserCount() int {
	OnlineUserMapLock.Lock()
	defer OnlineUserMapLock.Unlock()
	return len(OnlineUserMap)
}
