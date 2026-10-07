package release_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bengzyyys/batch-release/release"
)

// TestDocWorkflow 是 README“登记投料并查看数量核对”一段完整流程的可运行
// 版本：从一个尚未使用的数据位置开始，在同一个批次 B1 上完成登记配方、
// 创建批次、开始执行、两次投料、执行中查询、关闭、关闭后查询，以及草稿
// 投料、关闭后追加投料两类错误用法与原投料请求的幂等取回。
func TestDocWorkflow(t *testing.T) {
	dir := t.TempDir()

	s, err := release.Open(dir)
	if err != nil {
		t.Fatalf("打开台账失败: %v", err)
	}
	defer s.Close()

	// 登记配方 R1/v1：M1 每份 250 克，M2 每份 40 克。
	_, err = s.RegisterRecipe("req-001", "R1", "v1", "红枣配方", []release.MaterialInput{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M2", Grams: "40"},
	})
	if err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}

	// 创建批次 B1：使用 R1/v1，固定计划 2 份，创建后为草稿。
	_, err = s.CreateBatch("req-002", "B1", "R1", "v1", 2)
	if err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 错误用法一：草稿状态登记投料，返回 ErrInvalidState，不增加投料。
	if _, err = s.AddFeeding("req-bad-draft", "B1", "M1", "300",
		time.Date(2026, 10, 7, 8, 59, 0, 0, time.FixedZone("UTC+8", 8*60*60)), "张三"); !errors.Is(err, release.ErrInvalidState) {
		t.Fatalf("草稿投料应返回 ErrInvalidState，实际: %v", err)
	}

	// 草稿 → 执行中。
	if _, err = s.StartBatch("req-003", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}

	cst := time.FixedZone("UTC+8", 8*60*60)

	// M1 第一次投料 300 克。
	f1, err := s.AddFeeding("req-004", "B1", "M1", "300",
		time.Date(2026, 10, 7, 9, 0, 0, 0, cst), "张三")
	if err != nil {
		t.Fatalf("第一次投料失败: %v", err)
	}
	if f1.Seq != 1 || f1.MaterialNo != "M1" || f1.Grams != "300" || f1.Registrar != "张三" {
		t.Fatalf("第一次投料结果不符: %+v", f1)
	}

	// M1 第二次投料 150.5 克（千分之一克精度）。
	f2, err := s.AddFeeding("req-005", "B1", "M1", "150.5",
		time.Date(2026, 10, 7, 9, 20, 0, 0, cst), "李四")
	if err != nil {
		t.Fatalf("第二次投料失败: %v", err)
	}
	if f2.Seq != 2 || f2.Grams != "150.5" || f2.Registrar != "李四" {
		t.Fatalf("第二次投料结果不符: %+v", f2)
	}

	// 执行中查询：M1 应投 500、实投 450.5、差额 -49.5；
	// M2 未投料仍出现，应投 80、实投 0、差额 -80。
	executing, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("执行中查询失败: %v", err)
	}
	if executing.Status != release.StatusExecuting || executing.PlannedPortions != 2 ||
		executing.RecipeNo != "R1" || executing.RecipeVersion != "v1" {
		t.Fatalf("执行中批次头信息不符: %+v", executing)
	}
	if len(executing.Feedings) != 2 {
		t.Fatalf("执行中应有 2 条投料，实际 %d", len(executing.Feedings))
	}
	checkMaterials(t, executing.Materials, map[string][3]string{
		"M1": {"500", "450.5", "-49.5"},
		"M2": {"80", "0", "-80"},
	})

	// 执行中 → 已关闭：确认已有投料，数量不足也允许关闭。
	closed, err := s.CloseBatch("req-006", "B1")
	if err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}
	if closed.Status != release.StatusClosed {
		t.Fatalf("关闭结果状态应为 closed，实际 %q", closed.Status)
	}

	// 关闭后查询：配方版本、计划份数、投料顺序与核对数量与执行中一致，
	// 只有状态变为已关闭。
	after, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("关闭后查询失败: %v", err)
	}
	if after.Status != release.StatusClosed || after.PlannedPortions != 2 ||
		after.RecipeNo != "R1" || after.RecipeVersion != "v1" {
		t.Fatalf("关闭后批次头信息不符: %+v", after)
	}
	if len(after.Feedings) != 2 {
		t.Fatalf("关闭后应仍有 2 条投料，实际 %d", len(after.Feedings))
	}
	if after.Feedings[0].Seq != 1 || after.Feedings[0].Grams != "300" ||
		after.Feedings[1].Seq != 2 || after.Feedings[1].Grams != "150.5" {
		t.Fatalf("关闭后投料顺序或数量不符: %+v", after.Feedings)
	}
	checkMaterials(t, after.Materials, map[string][3]string{
		"M1": {"500", "450.5", "-49.5"},
		"M2": {"80", "0", "-80"},
	})

	// 错误用法二：关闭后用新请求编号追加投料，返回 ErrInvalidState，
	// 不增加投料。
	if _, err = s.AddFeeding("req-007", "B1", "M1", "100",
		time.Date(2026, 10, 7, 10, 0, 0, 0, cst), "张三"); !errors.Is(err, release.ErrInvalidState) {
		t.Fatalf("关闭后追加投料应返回 ErrInvalidState，实际: %v", err)
	}
	again, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("关闭后再次查询失败: %v", err)
	}
	if len(again.Feedings) != 2 {
		t.Fatalf("被拒绝的追加不应增加投料，实际 %d 条", len(again.Feedings))
	}

	// 原样重复提交已成功的投料请求 req-004：取回第一次登记结果，
	// 不是新增投料。
	replay, err := s.AddFeeding("req-004", "B1", "M1", "300",
		time.Date(2026, 10, 7, 9, 0, 0, 0, cst), "张三")
	if err != nil {
		t.Fatalf("幂等重放不应报错: %v", err)
	}
	if replay.Seq != 1 || replay.Grams != "300" || replay.Registrar != "张三" {
		t.Fatalf("重放应返回首次登记结果，实际: %+v", replay)
	}
	final, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("最终查询失败: %v", err)
	}
	if len(final.Feedings) != 2 {
		t.Fatalf("幂等重放不应增加投料，实际 %d 条", len(final.Feedings))
	}
}

func checkMaterials(t *testing.T, mats []release.MaterialRequirement, want map[string][3]string) {
	t.Helper()
	if len(mats) != len(want) {
		t.Fatalf("核对项数应为 %d，实际 %d: %+v", len(want), len(mats), mats)
	}
	for _, m := range mats {
		w, ok := want[m.MaterialNo]
		if !ok {
			t.Fatalf("出现多余物料 %q", m.MaterialNo)
		}
		if m.RequiredGrams != w[0] || m.ActualGrams != w[1] || m.DifferenceGrams != w[2] {
			t.Fatalf("物料 %q 核对不符：应投 %s 实投 %s 差额 %s，应为 应投 %s 实投 %s 差额 %s",
				m.MaterialNo, m.RequiredGrams, m.ActualGrams, m.DifferenceGrams, w[0], w[1], w[2])
		}
	}
}
