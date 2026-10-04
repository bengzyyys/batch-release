package release

import (
	"errors"
	"testing"
	"time"
)

// 幂等重放专用的两版配方：
//   - R1/v1：M1=0.125、M2=3（M2 仅在 v1 中）
//   - R1/v2：M1=0.5（同一物料每份克数不同）、M3=7（M3 仅在 v2 中）
// 共同物料 M1 每份克数不同，独有物料各不相同，
// 让两个计划的物料项目与数量核对都能明确区分。
func registerReplayRecipeVersions(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("recipe-rp-v1", "R1", "v1", "重放首版", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.125"},
		{MaterialNo: "M2", Grams: "3"},
	}); err != nil {
		t.Fatalf("登记 R1/v1 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("recipe-rp-v2", "R1", "v2", "重放改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "7"},
	}); err != nil {
		t.Fatalf("登记 R1/v2 失败: %v", err)
	}
}

// 核对第一次成功调整（R1/v1、8 份、草稿）的完整返回结果：
// 批次编号、配方编号、版本、名称、计划份数、草稿状态，
// 以及 v1 的物料项目与数量（M1 应投 0.125×8=1、M2 应投 3×8=24，
// 草稿无投料，实投为 0，差额为应投的负值，不含 v2 独有的 M3）。
func checkFirstAdjustResult(t *testing.T, view *BatchView) {
	t.Helper()
	if view.BatchNo != "B1" {
		t.Fatalf("历史结果的批次编号应为 B1，得到 %q", view.BatchNo)
	}
	if view.RecipeNo != "R1" || view.RecipeVersion != "v1" || view.RecipeName != "重放首版" {
		t.Fatalf("历史结果应指向 R1/v1（重放首版），得到 %q %q %q",
			view.RecipeNo, view.RecipeVersion, view.RecipeName)
	}
	if view.PlannedPortions != 8 {
		t.Fatalf("历史结果的计划份数应为 8，得到 %d", view.PlannedPortions)
	}
	if view.Status != StatusDraft {
		t.Fatalf("历史结果的状态应为草稿，得到 %s", view.Status)
	}
	if len(view.Feedings) != 0 {
		t.Fatalf("历史结果是草稿，不应有投料，得到 %d 条", len(view.Feedings))
	}
	mats := materialsMap(view)
	if len(mats) != 2 {
		t.Fatalf("历史结果应只含 v1 的 M1、M2 两种物料，得到 %d 项: %+v", len(mats), view.Materials)
	}
	checkRequirement(t, mats, "M1", "1", "0", "-1")   // 0.125 × 8
	checkRequirement(t, mats, "M2", "24", "0", "-24") // 3 × 8
	if _, leftover := mats["M3"]; leftover {
		t.Fatalf("v1 没有的物料 M3 不应出现在历史结果中: %+v", mats["M3"])
	}
}

// 核对当前批次仍保留后一次调整（R1/v2、5 份）的计划：
// M1 应投 0.5×5=2.5、M3 应投 7×5=35，不含 v1 独有的 M2。
func checkSecondAdjustPlan(t *testing.T, view *BatchView, status BatchStatus) {
	t.Helper()
	if view.BatchNo != "B1" || view.RecipeNo != "R1" || view.RecipeVersion != "v2" ||
		view.RecipeName != "重放改版" {
		t.Fatalf("当前批次应指向 R1/v2（重放改版），得到 %+v", view)
	}
	if view.PlannedPortions != 5 {
		t.Fatalf("当前批次的计划份数应为 5，得到 %d", view.PlannedPortions)
	}
	if view.Status != status {
		t.Fatalf("当前批次状态应为 %s，得到 %s", status, view.Status)
	}
	mats := materialsMap(view)
	if len(mats) != 2 {
		t.Fatalf("当前批次应只含 v2 的 M1、M3 两种物料，得到 %d 项: %+v", len(mats), view.Materials)
	}
	if _, leftover := mats["M2"]; leftover {
		t.Fatalf("v2 没有的物料 M2 不应残留: %+v", mats["M2"])
	}
}

// 草稿先成功改选配方版本并调整份数，随后又经另一次合法调整改用不同的
// 版本和份数。用第一次成功的请求编号、相同操作内容重新提交时，必须返回
// 第一次成功的结果，而不是最后一次调整后的数据；当前草稿仍保留后一次
// 调整的计划。
func TestUpdateDraftReplayReturnsFirstSuccessResult(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipeVersions(t, s)
	if _, err := s.CreateBatch("c1", "B1", "R1", "v2", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 第一次成功调整：改选 R1/v1 并改为 8 份。
	first, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("第一次调整应成功: %v", err)
	}
	checkFirstAdjustResult(t, first)

	// 第二次合法调整：改用 R1/v2、5 份，当前草稿以后一次为准。
	second, err := s.UpdateDraftBatch("u-second", "B1", "R1", "v2", 5)
	if err != nil {
		t.Fatalf("第二次调整应成功: %v", err)
	}
	checkSecondAdjustPlan(t, second, StatusDraft)
	secondMats := materialsMap(second)
	checkRequirement(t, secondMats, "M1", "2.5", "0", "-2.5") // 0.5 × 5
	checkRequirement(t, secondMats, "M3", "35", "0", "-35")   // 7 × 5

	// 重新提交第一次成功的请求（同一请求编号、相同内容）：
	// 返回的必须是那次调整的结果，不受后来调整影响。
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放第一次成功请求应成功: %v", err)
	}
	checkFirstAdjustResult(t, replay)

	// 当前草稿仍保留后一次调整的计划，不因返回历史结果而回退。
	cur, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询当前批次失败: %v", err)
	}
	checkSecondAdjustPlan(t, cur, StatusDraft)
	curMats := materialsMap(cur)
	checkRequirement(t, curMats, "M1", "2.5", "0", "-2.5")
	checkRequirement(t, curMats, "M3", "35", "0", "-35")
}

// 调整参数沿用原值的情况也要幂等：配方编号与版本同时留空表示保留当时的
// 配方，份数传 0 表示保留当时的份数。成功的请求重复提交时，应保留第一次
// 成功时已经确定的完整结果，不能拿留空参数去重新套用批次后来采用的
// 配方或份数。
func TestUpdateDraftReplayKeepsInheritedValues(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipeVersions(t, s)
	if _, err := s.CreateBatch("c1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 配方留空、只改份数：成功时确定为 R1/v1、8 份。
	keepRecipe, err := s.UpdateDraftBatch("u-keep-recipe", "B1", "", "", 8)
	if err != nil {
		t.Fatalf("配方留空的调整应成功: %v", err)
	}
	if keepRecipe.RecipeNo != "R1" || keepRecipe.RecipeVersion != "v1" || keepRecipe.PlannedPortions != 8 {
		t.Fatalf("配方留空应沿用当时的 R1/v1，得到 %+v", keepRecipe)
	}

	// 份数传 0、只改配方：成功时确定为 R1/v2、8 份（份数沿用当时的 8）。
	keepPortions, err := s.UpdateDraftBatch("u-keep-portions", "B1", "R1", "v2", 0)
	if err != nil {
		t.Fatalf("份数传 0 的调整应成功: %v", err)
	}
	if keepPortions.RecipeNo != "R1" || keepPortions.RecipeVersion != "v2" || keepPortions.PlannedPortions != 8 {
		t.Fatalf("份数传 0 应沿用当时的 8 份，得到 %+v", keepPortions)
	}

	// 批次后来改用另一配方与份数：R1/v1、3 份。
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v1", 3); err != nil {
		t.Fatalf("后续调整应成功: %v", err)
	}

	// 重放“配方留空”的请求：必须返回第一次成功时确定的 R1/v1、8 份，
	// 不能拿留空的配方参数去套用批次当前的配方（此时恰好也是 v1，
	// 但份数 8 与当前 3 不同，结果必须仍是 8）。
	replayRecipe, err := s.UpdateDraftBatch("u-keep-recipe", "B1", "", "", 8)
	if err != nil {
		t.Fatalf("重放配方留空的成功请求应成功: %v", err)
	}
	checkFirstAdjustResult(t, replayRecipe)

	// 重放“份数传 0”的请求：必须返回第一次成功时确定的 R1/v2、8 份，
	// 不能拿 0 份去沿用批次当前的 3 份。
	replayPortions, err := s.UpdateDraftBatch("u-keep-portions", "B1", "R1", "v2", 0)
	if err != nil {
		t.Fatalf("重放份数传 0 的成功请求应成功: %v", err)
	}
	if replayPortions.RecipeNo != "R1" || replayPortions.RecipeVersion != "v2" ||
		replayPortions.RecipeName != "重放改版" || replayPortions.PlannedPortions != 8 ||
		replayPortions.Status != StatusDraft {
		t.Fatalf("重放结果应为第一次成功时的 R1/v2、8 份、草稿，得到 %+v", replayPortions)
	}
	rpMats := materialsMap(replayPortions)
	if len(rpMats) != 2 {
		t.Fatalf("重放结果应只含 v2 的 M1、M3，得到 %d 项: %+v", len(rpMats), replayPortions.Materials)
	}
	checkRequirement(t, rpMats, "M1", "4", "0", "-4")   // 0.5 × 8
	checkRequirement(t, rpMats, "M3", "56", "0", "-56") // 7 × 8

	// 当前批次仍是后来的 R1/v1、3 份，不被重放改动。
	cur, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询当前批次失败: %v", err)
	}
	if cur.RecipeVersion != "v1" || cur.PlannedPortions != 3 || cur.Status != StatusDraft {
		t.Fatalf("当前批次应保留后来的 R1/v1、3 份、草稿，得到 %+v", cur)
	}
	curMats := materialsMap(cur)
	checkRequirement(t, curMats, "M1", "0.375", "0", "-0.375") // 0.125 × 3
	checkRequirement(t, curMats, "M2", "9", "0", "-9")         // 3 × 3
}

// 批次按后来的计划开始执行、登记投料并关闭后，重复提交同一成功调整仍应
// 成功返回原来的草稿结果；随后 GetBatch 看到的应是已关闭的实际批次，
// 仍保留后来确定的配方、份数、投料顺序及逐物料数量核对，
// 不得因为返回历史结果而回退状态或改写投料。
func TestUpdateDraftReplayAfterBatchClosed(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipeVersions(t, s)
	if _, err := s.CreateBatch("c1", "B1", "R1", "v2", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 第一次成功调整：R1/v1、8 份；随后第二次调整：R1/v2、5 份。
	first, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("第一次调整应成功: %v", err)
	}
	checkFirstAdjustResult(t, first)
	if _, err := s.UpdateDraftBatch("u-second", "B1", "R1", "v2", 5); err != nil {
		t.Fatalf("第二次调整应成功: %v", err)
	}

	// 按后来的计划（R1/v2、5 份）开始执行并登记投料：
	// 顺序为 M3、M1、M3，累计 M1=2.5、M3=35，恰好吻合应投。
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	feedTime := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("f1", "B1", "M3", "10", feedTime, "张三"); err != nil {
		t.Fatalf("登记第 1 条投料失败: %v", err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M1", "2.5", feedTime.Add(time.Hour), "李四"); err != nil {
		t.Fatalf("登记第 2 条投料失败: %v", err)
	}
	if _, err := s.AddFeeding("f3", "B1", "M3", "25", feedTime.Add(2*time.Hour), "张三"); err != nil {
		t.Fatalf("登记第 3 条投料失败: %v", err)
	}
	if _, err := s.CloseBatch("c9", "B1"); err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}

	// 批次已关闭，重复提交第一次成功调整仍应成功，返回原来的草稿结果。
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("批次关闭后重放第一次成功请求仍应成功: %v", err)
	}
	checkFirstAdjustResult(t, replay)

	// GetBatch 看到的是已关闭的实际批次：后来确定的配方与份数、
	// 投料按登记顺序排列、逐物料数量核对吻合，没有回退或改写。
	closed, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询已关闭批次失败: %v", err)
	}
	checkSecondAdjustPlan(t, closed, StatusClosed)
	if len(closed.Feedings) != 3 {
		t.Fatalf("已关闭批次应有 3 条投料，得到 %d 条", len(closed.Feedings))
	}
	wantFeedings := []FeedingView{
		{Seq: 1, MaterialNo: "M3", Grams: "10", Time: feedTime, Registrar: "张三"},
		{Seq: 2, MaterialNo: "M1", Grams: "2.5", Time: feedTime.Add(time.Hour), Registrar: "李四"},
		{Seq: 3, MaterialNo: "M3", Grams: "25", Time: feedTime.Add(2 * time.Hour), Registrar: "张三"},
	}
	for i, want := range wantFeedings {
		got := closed.Feedings[i]
		if got.Seq != want.Seq || got.MaterialNo != want.MaterialNo || got.Grams != want.Grams ||
			!got.Time.Equal(want.Time) || got.Registrar != want.Registrar {
			t.Fatalf("第 %d 条投料应为 %+v，得到 %+v", i+1, want, got)
		}
	}
	closedMats := materialsMap(closed)
	checkRequirement(t, closedMats, "M1", "2.5", "2.5", "0") // 0.5 × 5，实投 2.5
	checkRequirement(t, closedMats, "M3", "35", "35", "0")    // 7 × 5，实投 10+25
}

// 两种拒绝要区分开：沿用已成功的调整请求编号却更改调整内容，返回
// ErrRequestConflict；换用尚未使用的请求编号，对已关闭批次提交那次调整
// 的相同内容，返回 ErrInvalidState。两种拒绝都保留当前批次，
// 且原成功请求仍可取得原结果。
func TestUpdateDraftReplayConflictVsInvalidState(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipeVersions(t, s)
	if _, err := s.CreateBatch("c1", "B1", "R1", "v2", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); err != nil {
		t.Fatalf("第一次调整应成功: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-second", "B1", "R1", "v2", 5); err != nil {
		t.Fatalf("第二次调整应成功: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if _, err := s.CloseBatch("c9", "B1"); err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}

	// 同一请求编号、不同调整内容：请求编号冲突。
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 9); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同内容应返回 ErrRequestConflict，得到 %v", err)
	}
	// 同一请求编号、不同配方版本也属于不同内容。
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v2", 8); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号改配方应返回 ErrRequestConflict，得到 %v", err)
	}

	// 新请求编号、相同调整内容，但批次已关闭：状态不允许。
	if _, err := s.UpdateDraftBatch("u-fresh", "B1", "R1", "v1", 8); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号对已关闭批次应返回 ErrInvalidState，得到 %v", err)
	}

	// 两种拒绝都不改动当前批次：仍是已关闭的 R1/v2、5 份。
	cur, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询当前批次失败: %v", err)
	}
	checkSecondAdjustPlan(t, cur, StatusClosed)
	curMats := materialsMap(cur)
	checkRequirement(t, curMats, "M1", "2.5", "0", "-2.5")
	checkRequirement(t, curMats, "M3", "35", "0", "-35")

	// 原成功请求用相同内容重放，仍可取得原结果。
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("拒绝之后原成功请求仍应可重放: %v", err)
	}
	checkFirstAdjustResult(t, replay)
}
