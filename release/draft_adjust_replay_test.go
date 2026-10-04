package release

import (
	"errors"
	"testing"
	"time"
)

// 幂等重放专用的两版配方：
//   - R1/v1：M1=0.125、M2=0.5（M2 仅在 v1 中）
//   - R1/v2：M1=250（同一物料每份克数不同）、M4=2（M4 仅在 v2 中）
// 共同物料 M1 每份克数不同，各自又有独有物料，
// 两个计划返回的物料项目与数量都能明确区分。
func registerReplayRecipes(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("recipe-v1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.125"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatalf("登记 R1/v1 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("recipe-v2", "R1", "v2", "配方改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M4", Grams: "2"},
	}); err != nil {
		t.Fatalf("登记 R1/v2 失败: %v", err)
	}
}

// 第一次成功调整（R1/v1、8 份）应返回的完整结果：
// M1 = 0.125 × 8 = 1，M2 = 0.5 × 8 = 4，实投为零，差额为应投的负值。
func checkFirstAdjustView(t *testing.T, view *BatchView) {
	t.Helper()
	if view.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", view.BatchNo)
	}
	if view.RecipeNo != "R1" || view.RecipeVersion != "v1" || view.RecipeName != "配方初版" {
		t.Fatalf("应指向第一次调整的 R1/v1，得到 %q %q %q",
			view.RecipeNo, view.RecipeVersion, view.RecipeName)
	}
	if view.PlannedPortions != 8 {
		t.Fatalf("计划份数应为第一次调整的 8，得到 %d", view.PlannedPortions)
	}
	if view.Status != StatusDraft {
		t.Fatalf("第一次调整的结果应为草稿，得到 %s", view.Status)
	}
	if len(view.Feedings) != 0 {
		t.Fatalf("第一次调整的结果不应有投料，得到 %d 条", len(view.Feedings))
	}
	mats := materialsMap(view)
	if len(mats) != 2 {
		t.Fatalf("第一次调整的结果只应有 M1、M2 两种物料，得到 %d 项: %+v", len(mats), view.Materials)
	}
	checkRequirement(t, mats, "M1", "1", "0", "-1") // 0.125 × 8
	checkRequirement(t, mats, "M2", "4", "0", "-4") // 0.5 × 8
	if _, leftover := mats["M4"]; leftover {
		t.Fatalf("第一次调整的结果不应包含后来版本的物料 M4: %+v", mats["M4"])
	}
}

// 草稿先成功调整为 R1/v1、8 份，随后又经另一次合法调整改用 R1/v2、3 份。
// 用第一次的请求编号与相同内容重新提交，必须返回第一次成功的完整结果
// （批次编号、配方编号、版本、名称、计划份数、草稿状态与当时的数量核对），
// 而不是最后一次调整后的数据；当前草稿仍保留后一次调整的计划。
func TestUpdateDraftReplayReturnsFirstSuccessResult(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v2", 5); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	first, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("第一次调整应成功: %v", err)
	}
	checkFirstAdjustView(t, first)

	// 另一次合法调整：不同版本、不同份数。
	second, err := s.UpdateDraftBatch("u-second", "B1", "R1", "v2", 3)
	if err != nil {
		t.Fatalf("第二次调整应成功: %v", err)
	}
	if second.RecipeVersion != "v2" || second.PlannedPortions != 3 {
		t.Fatalf("第二次调整结果不正确: %+v", second)
	}

	// 重新提交第一次成功的请求：返回当时的完整结果，不受后来调整影响。
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重复提交第一次成功的请求应成功: %v", err)
	}
	checkFirstAdjustView(t, replay)

	// 再次重复提交，结果仍稳定。
	replay2, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("再次重复提交应成功: %v", err)
	}
	checkFirstAdjustView(t, replay2)

	// 当前草稿仍保留后一次调整的计划：R1/v2、3 份。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询批次失败: %v", err)
	}
	if got.RecipeNo != "R1" || got.RecipeVersion != "v2" || got.RecipeName != "配方改版" ||
		got.PlannedPortions != 3 || got.Status != StatusDraft {
		t.Fatalf("当前草稿应保留后一次调整的计划（R1/v2、3 份、草稿），得到 %+v", got)
	}
	mats := materialsMap(got)
	if len(mats) != 2 {
		t.Fatalf("当前草稿只应有 M1、M4 两种物料，得到 %d 项", len(mats))
	}
	checkRequirement(t, mats, "M1", "750", "0", "-750") // 250 × 3
	checkRequirement(t, mats, "M4", "6", "0", "-6")     // 2 × 3
}

// 调整参数沿用原值的成功请求，重复提交时也必须返回第一次成功时已确定的
// 完整结果，不能按批次后来采用的配方或份数重新套用：
//   - 配方编号与版本同时留空 = 保留当时的配方；
//   - 份数传 0 = 保留当时的份数。
func TestUpdateDraftReplayKeepsInheritedParameters(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 配方留空：保留当时的 R1/v1，只把份数改为 8。
	keepRecipe, err := s.UpdateDraftBatch("u-keep-recipe", "B1", "", "", 8)
	if err != nil {
		t.Fatalf("保留配方的调整应成功: %v", err)
	}
	checkFirstAdjustView(t, keepRecipe)

	// 份数传 0：保留当时的 8 份，只把配方改为 R1/v2。
	keepPortions, err := s.UpdateDraftBatch("u-keep-portions", "B1", "R1", "v2", 0)
	if err != nil {
		t.Fatalf("保留份数的调整应成功: %v", err)
	}
	if keepPortions.RecipeVersion != "v2" || keepPortions.PlannedPortions != 8 {
		t.Fatalf("保留份数的调整结果不正确: %+v", keepPortions)
	}
	kpMats := materialsMap(keepPortions)
	checkRequirement(t, kpMats, "M1", "2000", "0", "-2000") // 250 × 8
	checkRequirement(t, kpMats, "M4", "16", "0", "-16")      // 2 × 8

	// 批次后来再改为 4 份（配方不动）。
	if _, err := s.UpdateDraftBatch("u-later", "B1", "", "", 4); err != nil {
		t.Fatalf("后续调整应成功: %v", err)
	}

	// 重放“保留配方”的请求：结果必须是当时确定的 R1/v1、8 份，
	// 不能按当前批次的 R1/v2 重新套用留空参数。
	replayRecipe, err := s.UpdateDraftBatch("u-keep-recipe", "B1", "", "", 8)
	if err != nil {
		t.Fatalf("重放保留配方的请求应成功: %v", err)
	}
	checkFirstAdjustView(t, replayRecipe)

	// 重放“保留份数”的请求：结果必须是当时确定的 8 份，
	// 不能按当前批次的 4 份重新套用。
	replayPortions, err := s.UpdateDraftBatch("u-keep-portions", "B1", "R1", "v2", 0)
	if err != nil {
		t.Fatalf("重放保留份数的请求应成功: %v", err)
	}
	if replayPortions.RecipeNo != "R1" || replayPortions.RecipeVersion != "v2" ||
		replayPortions.PlannedPortions != 8 || replayPortions.Status != StatusDraft {
		t.Fatalf("重放结果应是第一次成功时的 R1/v2、8 份，得到 %+v", replayPortions)
	}
	rpMats := materialsMap(replayPortions)
	checkRequirement(t, rpMats, "M1", "2000", "0", "-2000")
	checkRequirement(t, rpMats, "M4", "16", "0", "-16")

	// 当前批次仍是后来确定的 R1/v2、4 份。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询批次失败: %v", err)
	}
	if got.RecipeVersion != "v2" || got.PlannedPortions != 4 || got.Status != StatusDraft {
		t.Fatalf("当前批次应保留后来的计划（R1/v2、4 份），得到 %+v", got)
	}
	gotMats := materialsMap(got)
	checkRequirement(t, gotMats, "M1", "1000", "0", "-1000") // 250 × 4
	checkRequirement(t, gotMats, "M4", "8", "0", "-8")       // 2 × 4
}

// 批次按后来的计划开始执行、登记投料并关闭后，重复提交同一成功调整
// 仍应成功返回原来的草稿结果；随后 GetBatch 看到的应是已关闭的实际批次，
// 保留后来确定的配方、份数、投料顺序及逐物料数量核对，
// 不因返回历史结果而回退状态或改写投料。重新打开台账后重放依然成立。
func TestUpdateDraftReplayAfterBatchClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v2", 5); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); err != nil {
		t.Fatalf("第一次调整应成功: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-second", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("第二次调整应成功: %v", err)
	}

	// 按后来的计划（R1/v2、3 份）执行、投料并关闭。
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	feedTime := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", feedTime, "张三"); err != nil {
		t.Fatalf("投料 f1 失败: %v", err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M4", "2", feedTime.Add(time.Hour), "李四"); err != nil {
		t.Fatalf("投料 f2 失败: %v", err)
	}
	if _, err := s.AddFeeding("f3", "B1", "M1", "50", feedTime.Add(2*time.Hour), "张三"); err != nil {
		t.Fatalf("投料 f3 失败: %v", err)
	}
	if _, err := s.CloseBatch("c1", "B1"); err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}

	// 关闭后重复提交第一次成功的调整：仍成功返回当时的草稿结果。
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("批次关闭后重放第一次成功的调整仍应成功: %v", err)
	}
	checkFirstAdjustView(t, replay)

	// 实际批次不被历史结果回退：已关闭，保留后来的配方、份数、
	// 投料顺序与逐物料数量核对。
	checkClosedBatch := func(got *BatchView) {
		t.Helper()
		if got.BatchNo != "B1" || got.RecipeNo != "R1" || got.RecipeVersion != "v2" ||
			got.RecipeName != "配方改版" || got.PlannedPortions != 3 || got.Status != StatusClosed {
			t.Fatalf("实际批次应为已关闭的 R1/v2、3 份，得到 %+v", got)
		}
		if len(got.Feedings) != 3 {
			t.Fatalf("应保留 3 条投料，得到 %d 条", len(got.Feedings))
		}
		wantFeedings := []FeedingView{
			{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: feedTime, Registrar: "张三"},
			{Seq: 2, MaterialNo: "M4", Grams: "2", Time: feedTime.Add(time.Hour), Registrar: "李四"},
			{Seq: 3, MaterialNo: "M1", Grams: "50", Time: feedTime.Add(2 * time.Hour), Registrar: "张三"},
		}
		for i, want := range wantFeedings {
			f := got.Feedings[i]
			if f.Seq != want.Seq || f.MaterialNo != want.MaterialNo || f.Grams != want.Grams ||
				!f.Time.Equal(want.Time) || f.Registrar != want.Registrar {
				t.Fatalf("第 %d 条投料应为 %+v，得到 %+v", i+1, want, f)
			}
		}
		mats := materialsMap(got)
		if len(mats) != 2 {
			t.Fatalf("已关闭批次只应有 M1、M4 两种物料，得到 %d 项", len(mats))
		}
		checkRequirement(t, mats, "M1", "750", "150.5", "-599.5") // 250×3，实投 100.5+50
		checkRequirement(t, mats, "M4", "6", "2", "-4")           // 2×3
	}
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询已关闭批次失败: %v", err)
	}
	checkClosedBatch(got)

	// 重新打开台账后：重放仍返回第一次成功的草稿结果，实际批次仍是已关闭状态。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	replay2, err := s2.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重新打开后重放第一次成功的调整仍应成功: %v", err)
	}
	checkFirstAdjustView(t, replay2)
	got2, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatalf("重新打开后查询批次失败: %v", err)
	}
	checkClosedBatch(got2)
}

// 两种拒绝必须区分：
//   - 沿用已成功的调整请求编号、却更改调整内容 → ErrRequestConflict；
//   - 换用尚未使用的请求编号、对已关闭批次提交相同内容 → ErrInvalidState。
// 两种拒绝都保留当前批次，且原成功请求仍可取得原结果。
func TestUpdateDraftReplayRejections(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v2", 5); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); err != nil {
		t.Fatalf("第一次调整应成功: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-second", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("第二次调整应成功: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if _, err := s.CloseBatch("c1", "B1"); err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}

	// 同一请求编号、不同调整内容（改了份数）→ ErrRequestConflict。
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 9); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同份数应返回 ErrRequestConflict，得到 %v", err)
	}
	// 同一请求编号、不同调整内容（改了配方版本）→ ErrRequestConflict。
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v2", 8); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同配方应返回 ErrRequestConflict，得到 %v", err)
	}
	// 新请求编号、相同内容，但批次已关闭 → ErrInvalidState。
	if _, err := s.UpdateDraftBatch("u-fresh", "B1", "R1", "v1", 8); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号对已关闭批次提交应返回 ErrInvalidState，得到 %v", err)
	}

	// 两种拒绝都不改动当前批次：仍是已关闭的 R1/v2、3 份。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询批次失败: %v", err)
	}
	if got.RecipeNo != "R1" || got.RecipeVersion != "v2" || got.PlannedPortions != 3 ||
		got.Status != StatusClosed {
		t.Fatalf("拒绝后批次应保持已关闭的 R1/v2、3 份，得到 %+v", got)
	}
	mats := materialsMap(got)
	checkRequirement(t, mats, "M1", "750", "0", "-750")
	checkRequirement(t, mats, "M4", "6", "0", "-6")

	// 原成功请求仍可取得原结果。
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("拒绝后原成功请求仍应可重放: %v", err)
	}
	checkFirstAdjustView(t, replay)
}
