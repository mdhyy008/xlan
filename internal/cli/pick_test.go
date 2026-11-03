package cli

import (
	"strings"
	"testing"

	"xlan/core"
)

func mkItems() []core.Item {
	return []core.Item{
		{ID: "1793721117349724086-a4105427", Status: core.StatusPending, Kind: core.KindFile},
		{ID: "1793720986339419397-82a020ee", Status: core.StatusDone, Kind: core.KindText},
		{ID: "1793720774919477967-4402ef56", Status: core.StatusPending, Kind: core.KindFile},
	}
}

func TestPickTargetNoArgSinglePending(t *testing.T) {
	items := []core.Item{{ID: "abc", Status: core.StatusPending}}
	got, err := pickTarget(items, "")
	if err != nil || got.ID != "abc" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestPickTargetNoArgNonePending(t *testing.T) {
	items := []core.Item{{ID: "a", Status: core.StatusDone}}
	if _, err := pickTarget(items, ""); err == nil {
		t.Fatal("没有待确认时应该报错")
	}
}

func TestPickTargetNoArgAmbiguous(t *testing.T) {
	_, err := pickTarget(mkItems(), "")
	if err == nil || !strings.Contains(err.Error(), "2 条") {
		t.Fatalf("多条待确认应该提示序号: %v", err)
	}
}

func TestPickTargetByIndex(t *testing.T) {
	items := mkItems()
	got, err := pickTarget(items, "1")
	if err != nil || got.ID != items[0].ID {
		t.Fatalf("序号 1 应命中第一条: %+v %v", got, err)
	}
	if _, err := pickTarget(items, "2"); err == nil {
		t.Fatal("第 2 条已完成，不该被选中")
	}
}

func TestPickTargetByPrefix(t *testing.T) {
	items := mkItems()
	got, err := pickTarget(items, "17937207")
	if err != nil || got.ID != items[2].ID {
		t.Fatalf("id 前缀匹配失败: %+v %v", got, err)
	}
	if _, err := pickTarget(items, "99999999"); err == nil {
		t.Fatal("不存在的前缀应该报错")
	}
}

func TestPickTargetIndexOutOfRangeFallsBackToPrefix(t *testing.T) {
	items := mkItems()
	// 99 超出列表范围，按 id 前缀找不到，应该报错而不是误选
	if _, err := pickTarget(items, "99"); err == nil {
		t.Fatal("越界序号不应选中任何条目")
	}
}
