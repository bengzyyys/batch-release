package release

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// StartBatch 幂等重放专用的两版配方，刻意让创建草稿时的旧计划与最终计划
// 在配方版本、名称、物料集合与每份克数上都能明确区分：
//   - R1/v1「旧版配方」：M1=100、M2=0.5、M3=0.010（M2、M3 仅在 v1 中）
//   - R1/v2「改版配方」：M1=12.5（同一物料每份克数不同）、M4=2（M4 仅在 v2 中）
//
// 草稿先按 v1 × 10 份创建（旧计划：M1=1000、M2=5、M3=0.1），
// 开始执行前再改选 v2 并调整为 4 份（最终计划：M1=50、M4=8）。
func registerStartReplayRecipes(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("recipe-v1", "R1", "v1", "旧版配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	}); err != nil {
		t.Fatalf("登记 R1/v1 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("recipe-v2", "R1", "v2", "改版配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "12.5"},
		{MaterialNo: "M4", Grams: "2"},
	}); err != nil {
		t.Fatalf("登记 R1/v2 失败: %v", err)
	}
}

// checkFirstStartView 校验“开始执行”首次成功时的原始结果：
// 必须使用最终选定的 R1/v2「改版配方」与最终份数 4，逐物料应投量按
// v2 的每份克数 × 4 计算（M1=12.5×4=50，M4=2×4=8），不能沿用创建
// 草稿时的 v1 × 10 旧计划（M1=1000、M2=5、M3=0.1 一律不得出现）。
// 首次开始时投料列表为空：各物料实投为零，差额为应投量的负值，
// 没有投料的 M4 仍要列出；状态为执行中。
func checkFirstStartView(t *testing.T, view *BatchView) {
	t.Helper()
	if view.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", view.BatchNo)
	}
	if view.RecipeNo != "R1" || view.RecipeVersion != "v2" || view.RecipeName != "改版配方" {
		t.Fatalf("开始结果应使用最终选定的 R1/v2「改版配方」，得到 %q %q %q",
			view.RecipeNo, view.RecipeVersion, view.RecipeName)
	}
	if view.PlannedPortions != 4 {
		t.Fatalf("开始结果应使用最终调整后的 4 份，得到 %d", view.PlannedPortions)
	}
	if view.Status != StatusExecuting {
		t.Fatalf("首次开始的结果应为执行中，得到 %s", view.Status)
	}
	if len(view.Feedings) != 0 {
		t.Fatalf("首次开始时投料列表应为空，得到 %d 条: %+v", len(view.Feedings), view.Feedings)
	}
	if len(view.Materials) != 2 {
		t.Fatalf("开始结果只应列出最终版本的 M1、M4 两种物料，得到 %d 项: %+v",
			len(view.Materials), view.Materials)
	}
	// 物料顺序与最终版本登记顺序一致。
	if view.Materials[0].MaterialNo != "M1" || view.Materials[1].MaterialNo != "M4" {
		t.Fatalf("数量核对应按最终版本物料顺序列出 M1、M4，得到 %+v", view.Materials)
	}
	mats := materialsMap(view)
	checkRequirement(t, mats, "M1", "50", "0", "-50") // 12.5 × 4
	checkRequirement(t, mats, "M4", "8", "0", "-8")   // 2 × 4
	for _, old := range []string{"M2", "M3"} {
		if _, leftover := mats[old]; leftover {
			t.Fatalf("开始结果不能沿用创建草稿时旧版本的物料 %s: %+v", old, mats[old])
		}
	}
}

// 投料、关闭后，正常查询应看到的当前台账：已关闭、最终计划不变、
// 保留 3 条投料与按当前实投计算的核对结果。
//
//	M1：应投 50，实投 30+5.5=35.5，差额 -14.5
//	M4：应投 8，实投 8，差额 0
var startReplayFeedTimes = [...]time.Time{
	time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC),
	time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC),
	time.Date(2026, 10, 1, 11, 30, 0, 0, time.UTC),
}

func checkClosedStartBatch(t *testing.T, got *BatchView) {
	t.Helper()
	if got.BatchNo != "B1" || got.RecipeNo != "R1" || got.RecipeVersion != "v2" ||
		got.RecipeName != "改版配方" || got.PlannedPortions != 4 || got.Status != StatusClosed {
		t.Fatalf("当前台账应为已关闭的 R1/v2、4 份，得到 %+v", got)
	}
	wantFeedings := []FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "30", Time: startReplayFeedTimes[0], Registrar: "张三"},
		{Seq: 2, MaterialNo: "M4", Grams: "8", Time: startReplayFeedTimes[1], Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", Grams: "5.5", Time: startReplayFeedTimes[2], Registrar: "张三"},
	}
	if len(got.Feedings) != len(wantFeedings) {
		t.Fatalf("应保留 %d 条投料，得到 %d 条: %+v", len(wantFeedings), len(got.Feedings), got.Feedings)
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
		t.Fatalf("已关闭批次只应有 M1、M4 两种物料，得到 %d 项: %+v", len(mats), got.Materials)
	}
	checkRequirement(t, mats, "M1", "50", "35.5", "-14.5")
	checkRequirement(t, mats, "M4", "8", "8", "0")
}

// 草稿改选物料用量不同的另一个已登记版本并调整份数后再开始执行：
// 开始结果必须采用最终计划；随后登记投料、关闭批次，再用同一请求编号
// 重复提交开始请求，必须原样返回首次成功时的结果（执行中、空投料、
// 零实投），不能重新执行状态变更，不能把已关闭批次改回执行中，
// 也不能混入后来追加的投料。正常查询则始终反映当前台账。
// 调用方对返回对象的修改、以及重新打开台账，都不能改变这些保证。
func TestStartBatchReplayReturnsFirstSuccessAfterClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStartReplayRecipes(t, s)

	// 草稿先按旧计划 v1 × 10 份创建。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	// 开始前改选 v2 并调整为 4 份——开始结果必须锁定这份最终计划。
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 4); err != nil {
		t.Fatalf("调整草稿失败: %v", err)
	}

	// 首次开始成功：返回执行中 + 空投料 + 按最终计划计算的数量核对。
	first, err := s.StartBatch("start-1", "B1")
	if err != nil {
		t.Fatalf("首次开始执行应成功: %v", err)
	}
	checkFirstStartView(t, first)

	// 调用方立即修改首次返回对象（状态、数量核对项、投料与物料列表），
	// 不能污染台账，也不能污染此后同一请求的重放结果。
	first.Status = StatusClosed
	first.PlannedPortions = 999
	first.RecipeNo = "R9"
	first.RecipeVersion = "v9"
	first.RecipeName = "被污染"
	first.Feedings = append(first.Feedings, FeedingView{Seq: 99, MaterialNo: "FAKE", Grams: "999"})
	first.Materials[0].ActualGrams = "9999"
	first.Materials[0].DifferenceGrams = "9999"
	first.Materials = append(first.Materials, MaterialRequirement{MaterialNo: "FAKE"})

	// 换用未使用的新请求编号再次开始“执行中”的批次 → ErrInvalidState，
	// 状态不变；同一时刻查询仍为执行中。
	if _, err := s.StartBatch("start-executing", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号开始执行中的批次应返回 ErrInvalidState，得到 %v", err)
	}

	// 随后登记三次投料，再关闭批次。
	if _, err := s.AddFeeding("f1", "B1", "M1", "30", startReplayFeedTimes[0], "张三"); err != nil {
		t.Fatalf("投料 f1 失败: %v", err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M4", "8", startReplayFeedTimes[1], "李四"); err != nil {
		t.Fatalf("投料 f2 失败: %v", err)
	}
	if _, err := s.AddFeeding("f3", "B1", "M1", "5.5", startReplayFeedTimes[2], "张三"); err != nil {
		t.Fatalf("投料 f3 失败: %v", err)
	}
	executing, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询执行中批次失败: %v", err)
	}
	if executing.Status != StatusExecuting || len(executing.Feedings) != 3 {
		t.Fatalf("关闭前查询应为执行中且已有 3 条投料，得到 %+v", executing)
	}
	if _, err := s.CloseBatch("close-1", "B1"); err != nil {
		t.Fatalf("关闭批次失败: %v", err)
	}

	// 记录重放前的当前台账。
	before, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("关闭后查询失败: %v", err)
	}
	checkClosedStartBatch(t, before)

	// 用原来的开始请求编号重复提交：返回首次成功的原始结果，
	// 不是当前台账——状态仍是执行中，投料列表仍为空，实投仍为零。
	replay, err := s.StartBatch("start-1", "B1")
	if err != nil {
		t.Fatalf("关闭后重复提交首次开始请求应成功重放: %v", err)
	}
	checkFirstStartView(t, replay)

	// 重放前后已保存的状态、配方、份数与投料记录完全不变；
	// 正常查询仍显示已关闭、已有投料与当前差额。
	after, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("重放后查询失败: %v", err)
	}
	checkClosedStartBatch(t, after)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("重放开始请求不应改动台账：重放前 %+v，重放后 %+v", before, after)
	}

	// 再次修改本次重放返回的对象，然后再查询、再重放：各自反映应有内容。
	replay.Status = StatusDraft
	replay.PlannedPortions = 7
	replay.Feedings = append(replay.Feedings, FeedingView{Seq: 1, MaterialNo: "M4", Grams: "8"})
	replay.Materials[0].RequiredGrams = "1000"
	replay.Materials = append(replay.Materials, MaterialRequirement{MaterialNo: "M2"})

	replay2, err := s.StartBatch("start-1", "B1")
	if err != nil {
		t.Fatalf("再次重复提交开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replay2)
	current, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("修改返回对象后查询失败: %v", err)
	}
	checkClosedStartBatch(t, current)

	// 重新打开台账后：开始请求仍重放首次成功的原始结果，当前批次仍已关闭。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	replay3, err := s2.StartBatch("start-1", "B1")
	if err != nil {
		t.Fatalf("重新打开后重放首次开始请求仍应成功: %v", err)
	}
	checkFirstStartView(t, replay3)
	reopened, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatalf("重新打开后查询批次失败: %v", err)
	}
	checkClosedStartBatch(t, reopened)
}

// 两种拒绝必须区分：
//   - 换一个未使用的请求编号开始已经执行或已经关闭的批次 → ErrInvalidState；
//   - 把首次开始成功的请求编号用于开始另一个仍为草稿的批次
//     （同操作、不同批次内容）→ ErrRequestConflict。
//
// 被拒绝后：另一个批次仍是草稿，原批次的状态与投料也不受影响；
// 原成功请求仍可取回首次开始的原始结果。
func TestStartBatchReplayRejections(t *testing.T) {
	s := openTestStore(t)
	registerStartReplayRecipes(t, s)

	// B1：按最终计划 v2 × 4 份开始，投料一次后关闭。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 4); err != nil {
		t.Fatalf("调整 B1 失败: %v", err)
	}
	if _, err := s.StartBatch("start-1", "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	feedTime := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("f1", "B1", "M1", "30", feedTime, "张三"); err != nil {
		t.Fatalf("投料失败: %v", err)
	}
	if _, err := s.CloseBatch("close-1", "B1"); err != nil {
		t.Fatalf("关闭 B1 失败: %v", err)
	}

	// B2：另一个仍为草稿的批次（使用不同计划，便于确认未被改动）。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 3); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	// B3：开始后停留在执行中。
	if _, err := s.CreateBatch("b3", "B3", "R1", "v2", 2); err != nil {
		t.Fatalf("创建 B3 失败: %v", err)
	}
	if _, err := s.StartBatch("start-b3", "B3"); err != nil {
		t.Fatalf("开始执行 B3 失败: %v", err)
	}

	// 新请求编号开始已关闭的 B1 → ErrInvalidState。
	if _, err := s.StartBatch("start-fresh-closed", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号开始已关闭批次应返回 ErrInvalidState，得到 %v", err)
	}
	// 新请求编号开始执行中的 B3 → ErrInvalidState。
	if _, err := s.StartBatch("start-fresh-executing", "B3"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号开始执行中批次应返回 ErrInvalidState，得到 %v", err)
	}
	// 首次成功的请求编号用于开始另一个草稿批次 B2 → ErrRequestConflict。
	if _, err := s.StartBatch("start-1", "B2"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("首次开始的请求编号用于另一批次应返回 ErrRequestConflict，得到 %v", err)
	}

	// 被拒绝后 B2 仍是原样草稿：计划未变，没有投料，没有被连带开始。
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("查询 B2 失败: %v", err)
	}
	if b2.Status != StatusDraft || b2.RecipeNo != "R1" || b2.RecipeVersion != "v1" ||
		b2.PlannedPortions != 3 || len(b2.Feedings) != 0 {
		t.Fatalf("被冲突拒绝后 B2 应仍是 v1、3 份的无投料草稿，得到 %+v", b2)
	}

	// B1 仍已关闭、保留投料；B3 仍执行中。
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询 B1 失败: %v", err)
	}
	if b1.Status != StatusClosed || b1.RecipeVersion != "v2" || b1.PlannedPortions != 4 {
		t.Fatalf("拒绝不应影响 B1，应仍为已关闭的 R1/v2、4 份，得到 %+v", b1)
	}
	if len(b1.Feedings) != 1 || b1.Feedings[0].MaterialNo != "M1" || b1.Feedings[0].Grams != "30" {
		t.Fatalf("B1 投料不应受拒绝影响，得到 %+v", b1.Feedings)
	}
	b3, err := s.GetBatch("B3")
	if err != nil {
		t.Fatalf("查询 B3 失败: %v", err)
	}
	if b3.Status != StatusExecuting || len(b3.Feedings) != 0 {
		t.Fatalf("B3 应仍为执行中且无投料，得到 %+v", b3)
	}

	// 原成功请求仍可取回首次开始的原始结果：执行中、空投料、零实投。
	replay, err := s.StartBatch("start-1", "B1")
	if err != nil {
		t.Fatalf("拒绝后原开始请求仍应可重放: %v", err)
	}
	checkFirstStartView(t, replay)

	// 重放不改变任何台账：B1 仍已关闭并保留投料，B2 仍草稿。
	b1Again, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("重放后查询 B1 失败: %v", err)
	}
	if b1Again.Status != StatusClosed || len(b1Again.Feedings) != 1 {
		t.Fatalf("重放开始请求不应改动 B1，得到 %+v", b1Again)
	}
	b2Again, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("重放后查询 B2 失败: %v", err)
	}
	if b2Again.Status != StatusDraft || b2Again.PlannedPortions != 3 {
		t.Fatalf("重放开始请求不应改动 B2，得到 %+v", b2Again)
	}
}
