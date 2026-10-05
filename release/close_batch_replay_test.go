package release

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“关闭批次”（CloseBatch）补充幂等重放回归测试。
// 现有行为：关闭只把执行中的批次变为已关闭，确认已经登记的投料，
// 不以数量吻合作为关闭条件，也不增加任何放行判断。本文件的回归保障
// 围绕这一行为展开，重点是：
//   - 关闭成功的返回结果与随后按批次查询的结果，都完整保留该批次实际
//     绑定的配方编号、版本、名称、计划份数，以及关闭前登记的全部投料；
//   - 不同物料穿插登记、填写时间先后不一致时，仍按登记顺序保留序号、
//     数量、时间与登记人，关闭时不按时间重排、不把多次投料合成一条；
//   - 逐物料核对保留真实差额，超投与欠投不互相抵消，关闭不把差额改零；
//   - 调用方拿到的关闭结果是数据副本，随意修改（含追加、清空列表）
//     不能改写已确认的台账，也不能污染首次成功关闭时保存的结果；
//   - 同一请求编号、同一批次重复提交关闭，返回第一次关闭时的完整原始
//     结果；换一个未使用的请求编号关闭已关闭批次，返回 ErrInvalidState。
//
// 复用 registerCloseRecipe 登记的配方：
//   - RC/v1（双料配方）：M1=0.1、M2=0.2
// 计划 3 份时应投 M1=0.3、M2=0.6；实际投料 M1 分两次 0.1+0.3=0.4（超投
// 0.1），M2 一次 0.5（欠投 0.1），两项差额一正一负，可以明确验证
// 关闭时既不互相抵消也不被改成零。

// registerCloseRecipe 登记关闭测试使用的双物料配方 RC/v1。
func registerCloseRecipe(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("recipe-rc", "RC", "v1", "双料配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.1"},
		{MaterialNo: "M2", Grams: "0.2"},
	}); err != nil {
		t.Fatalf("登记 RC/v1 失败: %v", err)
	}
}

// closeFeedTime 是关闭测试投料使用的固定时间基。
func closeFeedTime() time.Time {
	return time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
}

// wantCloseFeedings 是 B1 关闭前登记的全部投料，按登记顺序排列。
// 三条投料的填写时间故意与登记顺序不一致（第 2 条最早、第 1 条最晚），
// 且两种物料穿插登记：用于验证关闭结果与查询结果都按登记顺序保留
// 序号、数量、时间与登记人，不按时间重排，也不把同物料的两次投料
// 合并成一条。
func wantCloseFeedings() []FeedingView {
	base := closeFeedTime()
	return []FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "0.1", Time: base.Add(2 * time.Hour), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", Grams: "0.5", Time: base, Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", Grams: "0.3", Time: base.Add(time.Hour), Registrar: "张三"},
	}
}

// setupClosedBatch 准备并关闭批次 B1：登记 RC/v1，创建 3 份草稿，
// 开始执行，按 wantCloseFeedings 登记三条投料后用 "close-b1" 关闭。
// 返回首次关闭成功的结果。
func setupClosedBatch(t *testing.T, s *Store) *BatchView {
	t.Helper()
	registerCloseRecipe(t, s)
	if _, err := s.CreateBatch("b1", "B1", "RC", "v1", 3); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	for i, want := range wantCloseFeedings() {
		reqNo := []string{"f1", "f2", "f3"}[i]
		if _, err := s.AddFeeding(reqNo, "B1", want.MaterialNo, want.Grams, want.Time, want.Registrar); err != nil {
			t.Fatalf("投料 %s 失败: %v", reqNo, err)
		}
	}
	first, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("首次关闭应成功: %v", err)
	}
	return first
}

// checkClosedB1View 校验 B1 关闭后的完整视图（首次关闭的返回结果、
// 重放结果或随后按批次查询的结果都适用）：
//   - 保留实际绑定的配方编号、版本、名称（RC/v1、双料配方）与计划份数 3；
//   - 状态为已关闭；
//   - 完整保留关闭前三条投料的登记顺序、序号、数量、时间与登记人，
//     不按投料时间重排，同物料的两次投料（第 1、3 条）不合并；
//   - 逐物料核对保留真实差额：M1 应投 0.3、实投 0.4、差额 0.1，
//     M2 应投 0.6、实投 0.5、差额 -0.1；两项差额一正一负，
//     不互相抵消，也没有被关闭改成零。
func checkClosedB1View(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "RC" || v.RecipeVersion != "v1" || v.RecipeName != "双料配方" {
		t.Fatalf("应保留实际绑定的 RC/v1（双料配方），得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 3 {
		t.Fatalf("计划份数应为 3，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusClosed {
		t.Fatalf("状态应为已关闭，得到 %s", v.Status)
	}
	wantFeedings := wantCloseFeedings()
	if len(v.Feedings) != len(wantFeedings) {
		t.Fatalf("应保留 %d 条投料（同物料的两次投料不合并），得到 %d 条: %+v",
			len(wantFeedings), len(v.Feedings), v.Feedings)
	}
	for i, want := range wantFeedings {
		f := v.Feedings[i]
		if f.Seq != want.Seq || f.MaterialNo != want.MaterialNo || f.Grams != want.Grams ||
			!f.Time.Equal(want.Time) || f.Registrar != want.Registrar {
			t.Fatalf("第 %d 条投料应为 %+v（按登记顺序，不按时间重排），得到 %+v", i+1, want, f)
		}
	}
	mats := materialsMap(v)
	if len(mats) != 2 {
		t.Fatalf("应只有 M1、M2 两种物料，得到 %d 项: %+v", len(mats), v.Materials)
	}
	checkRequirement(t, mats, "M1", "0.3", "0.4", "0.1")   // 0.1×3；实投 0.1+0.3，超投不抵消 M2
	checkRequirement(t, mats, "M2", "0.6", "0.5", "-0.1")  // 0.2×3；实投 0.5，欠投不被 M1 抵消
}

// 关闭成功的返回结果与随后按批次查询的结果都完整保留配方绑定、计划份数
// 与关闭前全部投料；用同一请求编号、同一批次重复提交关闭，返回第一次
// 关闭时的完整原始结果。重新打开台账后，重放与查询仍各自反映应有的内容。
func TestCloseBatchResultPreservesFeedingsAndReconciliation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	first := setupClosedBatch(t, s)
	checkClosedB1View(t, first)

	// 随后按批次查询：与首次关闭结果一致，完整保留投料与真实差额。
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))

	// 同一请求编号、同一批次重复提交关闭：返回第一次关闭时的完整原始结果。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	checkClosedB1View(t, replay)

	// 再查询：重放不改变台账，内容仍与首次关闭一致。
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))

	// 重新打开台账后：重放仍返回首次关闭的原始结果，查询仍显示已关闭现状。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	replayAfterReopen, err := s2.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重新打开后重放关闭请求应成功: %v", err)
	}
	checkClosedB1View(t, replayAfterReopen)
	checkClosedB1View(t, mustGetBatch(t, s2, "B1"))
}

// 调用方修改首次关闭结果里的计划、状态、配方绑定、投料内容或物料核对项，
// 甚至追加、清空返回的列表，都不能改写已确认的台账；此后用同一请求编号
// 重复提交关闭仍返回第一次关闭时的完整原始结果。修改重放得到的结果后再
// 查询、再提交，仍应得到原有内容——返回的投料列表与核对列表都是调用方
// 取得的数据副本，修改它们只是本地整理，不能成为对已关闭批次的修改。
func TestCloseBatchResultIsCallerOwnedCopy(t *testing.T) {
	s := openTestStore(t)
	first := setupClosedBatch(t, s)
	checkClosedB1View(t, first)

	// 随意污染调用方拿到的首次关闭结果：状态、计划份数、配方绑定、
	// 投料条目内容、物料核对项，并追加伪造投料与伪造核对项。
	first.Status = StatusExecuting
	first.PlannedPortions = 99
	first.RecipeNo = "RX"
	first.RecipeVersion = "v9"
	first.RecipeName = "被污染的名称"
	first.Feedings[0].Grams = "999"
	first.Feedings[0].Registrar = "外人"
	first.Feedings = append(first.Feedings, FeedingView{
		Seq: 4, MaterialNo: "M1", Grams: "888", Time: closeFeedTime(), Registrar: "外人",
	})
	first.Materials[0].RequiredGrams = "1"
	first.Materials[0].ActualGrams = "0"
	first.Materials[0].DifferenceGrams = "0" // 试图把差额改成零
	first.Materials = append(first.Materials, MaterialRequirement{MaterialNo: "FAKE"})

	// 再提交同一关闭请求：重放结果必须仍是干净的首次原始结果。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("污染首次返回对象后重放应成功: %v", err)
	}
	checkClosedB1View(t, replay)

	// 再查询：台账本身未被污染，投料与差额保持首次关闭时的内容。
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))

	// 再污染重放对象：这次直接清空两个返回列表、改计划与状态。
	replay.Status = StatusDraft
	replay.PlannedPortions = 1
	replay.Feedings = nil
	replay.Materials = nil

	// 第三轮再提交、再查询：仍各自得到原有的完整内容。
	replay2, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("污染重放对象后再次重放应成功: %v", err)
	}
	checkClosedB1View(t, replay2)
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))
}

// 边界一：执行中尚无投料的批次仍可关闭。关闭结果的投料列表为空，
// 各物料实投为零、差额为负的应投量；随后查询与重放结果一致。
func TestCloseBatchWithNoFeedings(t *testing.T) {
	s := openTestStore(t)
	registerCloseRecipe(t, s)
	if _, err := s.CreateBatch("b1", "B1", "RC", "v1", 3); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}

	first, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("无投料的执行中批次应可关闭: %v", err)
	}
	checkEmptyClosed := func(v *BatchView) {
		t.Helper()
		if v.BatchNo != "B1" || v.RecipeNo != "RC" || v.RecipeVersion != "v1" ||
			v.RecipeName != "双料配方" || v.PlannedPortions != 3 || v.Status != StatusClosed {
			t.Fatalf("应为已关闭的 RC/v1、3 份，得到 %+v", v)
		}
		if len(v.Feedings) != 0 {
			t.Fatalf("无投料批次的投料列表应为空，得到 %d 条: %+v", len(v.Feedings), v.Feedings)
		}
		mats := materialsMap(v)
		if len(mats) != 2 {
			t.Fatalf("应列出 M1、M2 两种物料，得到 %d 项: %+v", len(mats), v.Materials)
		}
		checkRequirement(t, mats, "M1", "0.3", "0", "-0.3")
		checkRequirement(t, mats, "M2", "0.6", "0", "-0.6")
	}
	checkEmptyClosed(first)

	// 随后查询与同一请求编号重放，结果都与首次关闭一致。
	checkEmptyClosed(mustGetBatch(t, s, "B1"))
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	checkEmptyClosed(replay)
}

// 边界二：已关闭批次换一个未使用的请求编号再次关闭，返回 ErrInvalidState；
// 原有配方、计划、投料和核对结果保持不变，首次关闭的请求编号也不被影响。
// 同时保留错误分类：把首次关闭成功的请求编号用于另一批次属于
// ErrRequestConflict，而不是幂等重放。
func TestCloseBatchRejections(t *testing.T) {
	s := openTestStore(t)
	first := setupClosedBatch(t, s)
	checkClosedB1View(t, first)

	// 另一个仍为草稿的批次 B2，用于验证请求编号冲突分类。
	if _, err := s.CreateBatch("b2", "B2", "RC", "v1", 5); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}

	// 换一个未使用的请求编号再次关闭已关闭的 B1 → ErrInvalidState。
	if _, err := s.CloseBatch("close-again", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号关闭已关闭批次应返回 ErrInvalidState，得到 %v", err)
	}
	// 首次关闭成功的请求编号用于另一批次 → ErrRequestConflict。
	if _, err := s.CloseBatch("close-b1", "B2"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号关闭另一批次应返回 ErrRequestConflict，得到 %v", err)
	}

	// 被拒绝后 B1 的配方、计划、投料和核对结果保持不变。
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))

	// B2 仍是原样草稿：RC/v1、5 份、无投料、零实投。
	b2 := mustGetBatch(t, s, "B2")
	if b2.Status != StatusDraft || b2.RecipeNo != "RC" || b2.RecipeVersion != "v1" ||
		b2.RecipeName != "双料配方" || b2.PlannedPortions != 5 {
		t.Fatalf("被拒绝后 B2 应仍是 RC/v1、5 份的草稿，得到 %+v", b2)
	}
	if len(b2.Feedings) != 0 {
		t.Fatalf("B2 不应出现投料，得到 %d 条: %+v", len(b2.Feedings), b2.Feedings)
	}
	b2Mats := materialsMap(b2)
	checkRequirement(t, b2Mats, "M1", "0.5", "0", "-0.5") // 0.1 × 5
	checkRequirement(t, b2Mats, "M2", "1", "0", "-1")     // 0.2 × 5

	// 首次关闭的请求编号仍可取得第一次关闭时的完整原始结果。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("拒绝后原成功请求仍应可重放: %v", err)
	}
	checkClosedB1View(t, replay)
}
