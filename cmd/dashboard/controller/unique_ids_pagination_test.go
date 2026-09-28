package controller

import (
	"math"
	"slices"
	"testing"

	"github.com/nezhahq/nezha/model"
)

// 非相邻重复 id 必须被去掉，否则分组写入前的存在性计数会误报“have invalid ... id”。
func TestUniqueIDsDedupesNonAdjacent(t *testing.T) {
	got := uniqueIDs([]uint64{3, 1, 3, 2, 1})
	if !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("uniqueIDs = %v, want [1 2 3]", got)
	}
}

// 超大 limit 不能让 offset+limit 溢出成负数而 panic。
func TestPaginateOnlineHugeLimit(t *testing.T) {
	all := []*model.OnlineUser{{UserID: 1}, {UserID: 2}, {UserID: 3}}
	if got := paginateOnline(all, 1, math.MaxInt); len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got := paginateOnline(all, 0, 2); len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got := paginateOnline(all, 3, 10); got != nil {
		t.Fatalf("offset 越界应返回 nil，got %v", got)
	}
}
