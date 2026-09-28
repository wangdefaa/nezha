package geoip

import (
	_ "embed"
	"errors"
	"net"
	"strings"
	"sync"

	maxminddb "github.com/oschwald/maxminddb-golang"
)

//go:embed geoip.db
var db []byte

var (
	dbOnce = sync.OnceValues(func() (*maxminddb.Reader, error) {
		db, err := maxminddb.FromBytes(db)
		if err != nil {
			return nil, err
		}
		return db, nil
	})
)

// IPInfo 只解出 Lookup 实际用到的国家/大洲代码。
type IPInfo struct {
	Country   string `maxminddb:"country"`
	Continent string `maxminddb:"continent"`
}

func Lookup(ip net.IP) (string, error) {
	db, err := dbOnce()
	if err != nil {
		return "", err
	}

	var record IPInfo
	err = db.Lookup(ip, &record)
	if err != nil {
		return "", err
	}

	if record.Country != "" {
		return strings.ToLower(record.Country), nil
	} else if record.Continent != "" {
		return strings.ToLower(record.Continent), nil
	}

	return "", errors.New("IP not found")
}
