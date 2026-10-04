package release

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“开始执行批次”（StartBatch）补充幂等重放回归测试，
// 重点区分“某次成功请求的原始返回结果”与“批次当前的台账记录”：
// 用同一请求编号再次开始同一批次时，只能取回首次开始成功那一刻的结果；
// 批次后来的投料、关闭都不能混入该结果，也不能借重放重新执行状态变更
// （尤其不能把已关闭批次改回执行中）。
//
// 复用 registerReplayRecipes 登记的两版配方：
//   - R1/v1（配方初版）：M1=0.125、M2=0.5
//   - R1/v2（配方改版）：M1=250、M4=2
// 共同物料 M1 每份克数不同，各自又有独有物料，
// 最终计划与创建草稿时的旧计划能从物料项与数量上明确区分。

// replayFeedTime 是开始执行后追加投料使用的固定时间基。
func replayFeedTime() time.Time {
	return time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
}

// checkFirstStartView 校验“开始执行 B1”首次成功时的原始结果。
// 场景前置：B1 创建时先用 R1/v1、5 份，草稿调整为 R1/v2、3 份后开始。
// 原始结果必须：
//   - 使用最终选定的配方编号、版本、名称（R1/v2、配方改版）与最终份数 3，
//     不能沿用创建草稿时 R1/v1 的旧计划（不能出现旧物料 M2，M1 应投按 250×3）；
//   - 状态为执行中；
//   - 投料列表为空；
//   - 各物料按最终版本的每份克数 × 最终份数列出应投量，实投为零、
//     差额为应投量的负值；没有投料的 M4 仍要列出。
func checkFirstStartView(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "R1" || v.RecipeVersion != "v2" || v.RecipeName != "配方改版" {
		t.Fatalf("开始结果应使用最终选定的 R1/v2（配方改版），得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 3 {
		t.Fatalf("计划份数应为最终调整的 3，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusExecuting {
		t.Fatalf("首次开始结果应为执行中，得到 %s", v.Status)
	}
	if len(v.Feedings) != 0 {
		t.Fatalf("首次开始时投料列表应为空，得到 %d 条: %+v", len(v.Feedings), v.Feedings)
	}
	if len(v.Materials) != 2 {
		t.Fatalf("应只列出最终版本 v2 的 M1、M4 两种物料，得到 %d 项: %+v",
			len(v.Materials), v.Materials)
	}
	// 物料顺序与最终配方一致，数量按 v2 每份克数 × 最终 3 份：
	// M1 = 250 × 3 = 750（不是旧版 0.125 × 5），M4 = 2 × 3 = 6。
	if v.Materials[0].MaterialNo != "M1" || v.Materials[1].MaterialNo != "M4" {
		t.Fatalf("物料顺序应与 R1/v2 一致（M1、M4），得到 %+v", v.Materials)
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "M1", "750", "0", "-750")
	checkRequirement(t, mats, "M4", "6", "0", "-6")
	if _, leftover := mats["M2"]; leftover {
		t.Fatalf("开始结果不能沿用创建草稿时旧版本 v1 的物料 M2: %+v", mats["M2"])
	}
}

// checkClosedCurrent 校验 B1 投料并关闭后的“当前台账记录”：
// 仍绑定 R1/v2、3 份、已关闭；保留 3 条按登记顺序排列的投料；
// 数量核对反映实际投料——M1 实投 100.5+50=150.5，M4 实投 2。
func checkClosedCurrent(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" || v.RecipeNo != "R1" || v.RecipeVersion != "v2" ||
		v.RecipeName != "配方改版" || v.PlannedPortions != 3 || v.Status != StatusClosed {
		t.Fatalf("当前台账应为已关闭的 R1/v2、3 份，得到 %+v", v)
	}
	base := replayFeedTime()
	wantFeedings := []FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: base, Registrar: "张三"},
		{Seq: 2, MaterialNo: "M4", Grams: "2", Time: base.Add(time.Hour), Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", Grams: "50", Time: base.Add(2 * time.Hour), Registrar: "张三"},
	}
	if len(v.Feedings) != len(wantFeedings) {
		t.Fatalf("当前台账应保留 %d 条投料，得到 %d 条: %+v",
			len(wantFeedings), len(v.Feedings), v.Feedings)
	}
	for i, want := range wantFeedings {
		f := v.Feedings[i]
		if f.Seq != want.Seq || f.MaterialNo != want.MaterialNo || f.Grams != want.Grams ||
			!f.Time.Equal(want.Time) || f.Registrar != want.Registrar {
			t.Fatalf("第 %d 条投料应为 %+v，得到 %+v", i+1, want, f)
		}
	}
	mats := materialsMap(v)
	if len(mats) != 2 {
		t.Fatalf("当前台账只应有 M1、M4 两种物料，得到 %d 项: %+v", len(mats), v.Materials)
	}
	checkRequirement(t, mats, "M1", "750", "150.5", "-599.5") // 250×3；实投 100.5+50
	checkRequirement(t, mats, "M4", "6", "2", "-4")           // 2×3；实投 2
}

// 草稿先改选物料用量不同的已登记版本并调整计划份数，再开始执行；
// 开始结果必须按最终选定版本与最终份数计算。随后投料、关闭，再用同一
// 请求编号重复开始：每次都返回首次成功时的原始结果（执行中、空投料、
// 零实投），不混入后来的投料，不重新执行状态变更，台账也不被改回执行中。
// 重新打开台账后，重放与查询仍各自反映应有的内容。
func TestStartBatchReplayReturnsFirstResultAfterFeedingAndClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s)

	// 创建草稿时先使用旧版本 R1/v1、5 份，随后调整为最终计划 R1/v2、3 份。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("调整草稿失败: %v", err)
	}

	first, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("首次开始执行应成功: %v", err)
	}
	checkFirstStartView(t, first)

	// 执行中登记两笔投料。
	base := replayFeedTime()
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", base, "张三"); err != nil {
		t.Fatalf("投料 f1 失败: %v", err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M4", "2", base.Add(time.Hour), "李四"); err != nil {
		t.Fatalf("投料 f2 失败: %v", err)
	}

	// 批次已有投料、仍在执行中时重复提交首次开始请求：
	// 返回的仍是首次开始时的原始结果（空投料、执行中），不重复状态变更。
	replayExec, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("投料后重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replayExec)

	// 正常查询反映的是当前台账：执行中、已有 2 条投料与当前差额。
	execView, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询执行中批次失败: %v", err)
	}
	if execView.Status != StatusExecuting || execView.PlannedPortions != 3 ||
		execView.RecipeVersion != "v2" {
		t.Fatalf("当前批次应为执行中的 R1/v2、3 份，得到 %+v", execView)
	}
	if len(execView.Feedings) != 2 {
		t.Fatalf("当前批次应已有 2 条投料，得到 %d 条", len(execView.Feedings))
	}
	execMats := materialsMap(execView)
	checkRequirement(t, execMats, "M1", "750", "100.5", "-649.5")
	checkRequirement(t, execMats, "M4", "6", "2", "-4")

	// 再登记一笔投料并关闭批次。
	if _, err := s.AddFeeding("f3", "B1", "M1", "50", base.Add(2*time.Hour), "张三"); err != nil {
		t.Fatalf("投料 f3 失败: %v", err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}

	// 关闭后重复提交原来的开始请求：仍返回首次成功的原始结果，
	// 不能重新执行状态变更，更不能把已关闭批次改回执行中。
	replayClosed, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("关闭后重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replayClosed)
	checkClosedCurrent(t, mustGetBatch(t, s, "B1"))

	// 再提交与再查询交替进行：重放结果始终是首次原始结果，
	// 查询始终是已关闭的当前台账；重复提交前后台账保持不变。
	replayAgain, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("再次重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replayAgain)
	checkClosedCurrent(t, mustGetBatch(t, s, "B1"))

	// 重新打开台账后：重放仍返回首次开始时的原始结果，查询仍显示已关闭现状。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	replayAfterReopen, err := s2.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重新打开后重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replayAfterReopen)
	checkClosedCurrent(t, mustGetBatch(t, s2, "B1"))
}

// 调用方修改首次开始返回对象中的状态、物料核对项或列表内容，
// 不能污染台账，也不能污染此后同一请求的重放结果；
// 再查询与再提交应各自反映应有的内容。
func TestStartBatchReplayResultIsCallerOwnedCopy(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("调整草稿失败: %v", err)
	}
	first, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("首次开始执行应成功: %v", err)
	}
	checkFirstStartView(t, first)

	// 随意污染调用方拿到的首次返回对象：状态、配方、份数、物料核对、
	// 投料列表、新增伪造项。
	first.Status = StatusClosed
	first.PlannedPortions = 99
	first.RecipeNo = "RX"
	first.RecipeVersion = "v1"
	first.RecipeName = "被污染的名称"
	first.Feedings = append(first.Feedings, FeedingView{
		Seq: 1, MaterialNo: "M1", Grams: "999", Time: replayFeedTime(), Registrar: "外人",
	})
	first.Materials[0].RequiredGrams = "1"
	first.Materials[0].ActualGrams = "999"
	first.Materials[0].DifferenceGrams = "998"
	first.Materials = append(first.Materials, MaterialRequirement{MaterialNo: "FAKE"})

	// 再提交同一请求：重放结果必须仍是干净的首次原始结果。
	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("污染首次返回对象后重放应成功: %v", err)
	}
	checkFirstStartView(t, replay)

	// 再查询：台账本身未被污染——执行中、最终计划、空投料、零实投。
	got := mustGetBatch(t, s, "B1")
	if got.Status != StatusExecuting || got.RecipeNo != "R1" ||
		got.RecipeVersion != "v2" || got.RecipeName != "配方改版" ||
		got.PlannedPortions != 3 {
		t.Fatalf("台账被首次返回对象污染: %+v", got)
	}
	if len(got.Feedings) != 0 {
		t.Fatalf("台账不应出现伪造投料，得到 %d 条: %+v", len(got.Feedings), got.Feedings)
	}
	gotMats := materialsMap(got)
	if len(gotMats) != 2 {
		t.Fatalf("台账物料项被污染，得到 %d 项: %+v", len(gotMats), got.Materials)
	}
	checkRequirement(t, gotMats, "M1", "750", "0", "-750")
	checkRequirement(t, gotMats, "M4", "6", "0", "-6")

	// 再污染重放对象，第二轮再提交、再查询仍各自干净。
	replay.Status = StatusDraft
	replay.Materials[0].ActualGrams = "42"
	replay.Feedings = append(replay.Feedings, FeedingView{Seq: 2, MaterialNo: "FAKE", Grams: "7"})
	replay2, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("污染重放对象后再次重放应成功: %v", err)
	}
	checkFirstStartView(t, replay2)
	got2 := mustGetBatch(t, s, "B1")
	if got2.Status != StatusExecuting || len(got2.Feedings) != 0 {
		t.Fatalf("台账被重放对象污染: 状态=%s 投料数=%d", got2.Status, len(got2.Feedings))
	}
	got2Mats := materialsMap(got2)
	checkRequirement(t, got2Mats, "M1", "750", "0", "-750")
	checkRequirement(t, got2Mats, "M4", "6", "0", "-6")
}

// 保留两种拒绝行为的区分：
//   - 换一个未使用的请求编号开始已经执行（或已关闭）的批次 → ErrInvalidState；
//   - 把首次成功的请求编号用于开始另一个仍为草稿的批次 → ErrRequestConflict
//     （操作相同但批次编号不同）。
// 被拒绝后，另一个批次仍是草稿，原批次的状态与投料也不受影响；
// 原成功请求仍可取得首次结果。
func TestStartBatchReplayRejections(t *testing.T) {
	s := openTestStore(t)
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("调整 B1 失败: %v", err)
	}
	// 另一个仍为草稿的批次 B2，沿用 R1/v1、7 份。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 7); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}

	first, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("首次开始 B1 应成功: %v", err)
	}
	checkFirstStartView(t, first)

	// 执行中：新请求编号再次开始 B1 → ErrInvalidState（不会被当成幂等重放）。
	if _, err := s.StartBatch("start-fresh-executing", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号开始执行中的批次应返回 ErrInvalidState，得到 %v", err)
	}
	// 首次成功的编号用于开始另一个草稿批次 B2 → ErrRequestConflict。
	if _, err := s.StartBatch("start-b1", "B2"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号开始另一批次应返回 ErrRequestConflict，得到 %v", err)
	}

	// 投料并关闭 B1。
	base := replayFeedTime()
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", base, "张三"); err != nil {
		t.Fatalf("投料 f1 失败: %v", err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M4", "2", base.Add(time.Hour), "李四"); err != nil {
		t.Fatalf("投料 f2 失败: %v", err)
	}
	if _, err := s.AddFeeding("f3", "B1", "M1", "50", base.Add(2*time.Hour), "张三"); err != nil {
		t.Fatalf("投料 f3 失败: %v", err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatalf("关闭 B1 失败: %v", err)
	}

	// 已关闭：新编号开始 B1 → ErrInvalidState，绝不能改回执行中。
	if _, err := s.StartBatch("start-fresh-closed", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号开始已关闭批次应返回 ErrInvalidState，得到 %v", err)
	}
	// 原成功编号用于 B2 仍是冲突。
	if _, err := s.StartBatch("start-b1", "B2"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("关闭后同编号开始另一批次仍应返回 ErrRequestConflict，得到 %v", err)
	}

	// 被拒绝后 B2 仍是原样草稿：R1/v1、7 份、无投料、零实投。
	b2 := mustGetBatch(t, s, "B2")
	if b2.Status != StatusDraft || b2.RecipeNo != "R1" || b2.RecipeVersion != "v1" ||
		b2.RecipeName != "配方初版" || b2.PlannedPortions != 7 {
		t.Fatalf("被拒绝后 B2 应仍是 R1/v1、7 份的草稿，得到 %+v", b2)
	}
	if len(b2.Feedings) != 0 {
		t.Fatalf("B2 不应出现投料，得到 %d 条: %+v", len(b2.Feedings), b2.Feedings)
	}
	b2Mats := materialsMap(b2)
	if len(b2Mats) != 2 {
		t.Fatalf("B2 应仍是 v1 的 M1、M2 两种物料，得到 %d 项: %+v", len(b2Mats), b2.Materials)
	}
	checkRequirement(t, b2Mats, "M1", "0.875", "0", "-0.875") // 0.125 × 7
	checkRequirement(t, b2Mats, "M2", "3.5", "0", "-3.5")     // 0.5 × 7

	// B1 仍是已关闭状态，投料与数量核对不变。
	checkClosedCurrent(t, mustGetBatch(t, s, "B1"))

	// 原成功请求仍可取得首次开始时的原始结果。
	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("拒绝后原成功请求仍应可重放: %v", err)
	}
	checkFirstStartView(t, replay)
}

// mustGetBatch 查询批次，失败时立即终止测试。
func mustGetBatch(t *testing.T, s *Store, batchNo string) *BatchView {
	t.Helper()
	v, err := s.GetBatch(batchNo)
	if err != nil {
		t.Fatalf("查询批次 %q 失败: %v", batchNo, err)
	}
	return v
}
