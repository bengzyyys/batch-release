package release

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“投料”（AddFeeding）补充请求编号与投料内容一致性的回归测试，
// 保护一笔已经成功登记的投料：
//   - 用原请求编号与原内容再次提交，只能取回第一次成功登记的那条记录，
//     即使其间已经追加了其他投料，也不能把最近一笔投料误当成原记录，
//     更不能重复计入实投量；
//   - 沿用已成功的请求编号时，批次、物料、克数字符串、投料时间、登记人
//     任一项改变，都必须返回可用 errors.Is 判断的 ErrRequestConflict，
//     且这些“不同内容”本身都符合投料条件（改投同一配方内的另一物料、
//     换一个合法数量、更换登记人等），不靠非法输入被拒绝来冒充冲突；
//   - 克数的显示格式与请求内容要分清：“1.000”成功后显示“1”，但再次
//     提交只有仍带“1.000”才算同一内容，改成数值相等的“1”仍是冲突；
//   - 请求编号在同一台账内共用，另一个执行中的批次也不能沿用；
//   - 冲突不覆盖、不追加、不占用下一个登记序号；冲突之后原内容仍能用原
//     编号取得第一次的结果，而换用新请求编号提交相同内容则是独立的一笔，
//     正常追加并取得连续的下一个序号。

// feedingReplayTime 是投料回归测试使用的固定时间基。
func feedingReplayTime() time.Time {
	return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
}

// checkFeedingView 逐条核对一条投料视图的序号、物料、显示克数、时间与登记人。
func checkFeedingView(t *testing.T, f *FeedingView, seq int, material, grams string, tm time.Time, registrar string) {
	t.Helper()
	if f.Seq != seq || f.MaterialNo != material || f.Grams != grams ||
		!f.Time.Equal(tm) || f.Registrar != registrar {
		t.Fatalf("投料视图应为 序号=%d 物料=%s 克数=%s 时间=%s 登记人=%s，得到 %+v",
			seq, material, grams, tm.Format(time.RFC3339), registrar, f)
	}
}

// 执行中的批次成功登记一笔投料后，使用原请求编号、原批次编号、原物料、
// 原克数字符串、原投料时间和原登记人再次提交，应返回第一次成功登记的那条
// 记录。即使其间已经追加了其他投料，返回的序号、物料、数量、时间和登记人
// 仍属于原记录，不能变成最近一笔；批次查询中的投料条数、登记顺序和各物料
// 累计实投量也不能因重放而变化。重新打开台账后，重放仍返回首次结果。
func TestAddFeedingReplayReturnsFirstRecordAfterMoreFeedings(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStandardRecipe(t, s, "recipe-1") // M1=100、M2=0.5、M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}

	t1 := feedingReplayTime()
	// 首次提交带“1.000”，成功结果的克数显示为“1”。
	first, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("首次登记投料应成功: %v", err)
	}
	checkFeedingView(t, first, 1, "M1", "1", t1, "张三")

	// 其间追加两笔其他投料（含另一物料与另一登记人）。
	t2 := t1.Add(time.Hour)
	t3 := t1.Add(2 * time.Hour)
	second, err := s.AddFeeding("feed-2", "B1", "M2", "2", t2, "李四")
	if err != nil {
		t.Fatalf("追加第二笔投料失败: %v", err)
	}
	checkFeedingView(t, second, 2, "M2", "2", t2, "李四")
	third, err := s.AddFeeding("feed-3", "B1", "M1", "3", t3, "王五")
	if err != nil {
		t.Fatalf("追加第三笔投料失败: %v", err)
	}
	checkFeedingView(t, third, 3, "M1", "3", t3, "王五")

	// 用原请求编号与完全相同的原内容再次提交：必须返回第一次的记录，
	// 不能变成最近一笔（序号 3、M1、3 克、王五）。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("原内容重放应成功并返回首次结果: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")

	// 批次查询不因子虚的重放而变化：仍是 3 条，登记顺序保持先后，
	// 各物料累计实投只统计真正追加的投料（M1=1+3=4，M2=2，M3=0）。
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询批次失败: %v", err)
	}
	wantFeedings := []FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "1", Time: t1, Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", Grams: "2", Time: t2, Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", Grams: "3", Time: t3, Registrar: "王五"},
	}
	if len(view.Feedings) != len(wantFeedings) {
		t.Fatalf("重放不应新增投料，应仍为 %d 条，得到 %d 条: %+v",
			len(wantFeedings), len(view.Feedings), view.Feedings)
	}
	for i, want := range wantFeedings {
		got := view.Feedings[i]
		if got.Seq != want.Seq || got.MaterialNo != want.MaterialNo || got.Grams != want.Grams ||
			!got.Time.Equal(want.Time) || got.Registrar != want.Registrar {
			t.Fatalf("第 %d 条投料应为 %+v，得到 %+v", i+1, want, got)
		}
	}
	mats := materialsMap(view)
	checkRequirement(t, mats, "M1", "1000", "4", "-996") // 100×10；实投 1+3
	checkRequirement(t, mats, "M2", "5", "2", "-3")      // 0.5×10；实投 2
	checkRequirement(t, mats, "M3", "0.1", "0", "-0.1")  // 0.010×10；无投料

	// 再多重放一次，条数与累计仍保持不变。
	replay2, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("再次重放应成功: %v", err)
	}
	checkFeedingView(t, replay2, 1, "M1", "1", t1, "张三")
	view, _ = s.GetBatch("B1")
	if len(view.Feedings) != 3 {
		t.Fatalf("重复重放不应新增投料，得到 %d 条", len(view.Feedings))
	}

	// 重新打开同一台账：请求结果完整可查，重放仍返回首次登记的记录，
	// 查询仍是 3 条投料与相同的累计实投。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	replayAfterReopen, err := s2.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("重新打开后重放应成功: %v", err)
	}
	checkFeedingView(t, replayAfterReopen, 1, "M1", "1", t1, "张三")
	reopened, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatalf("重新打开后查询失败: %v", err)
	}
	if len(reopened.Feedings) != 3 {
		t.Fatalf("重新打开后应仍为 3 条投料，得到 %d 条", len(reopened.Feedings))
	}
	reopenedMats := materialsMap(reopened)
	checkRequirement(t, reopenedMats, "M1", "1000", "4", "-996")
	checkRequirement(t, reopenedMats, "M2", "5", "2", "-3")
	checkRequirement(t, reopenedMats, "M3", "0.1", "0", "-0.1")
}

// 沿用已成功使用的请求编号时，批次、物料、克数字符串、投料时间或登记人
// 任一项改变，都必须返回 ErrRequestConflict（可用 errors.Is 判断）。
// 这里每个改动本身都符合投料条件：改投同一配方中的另一种物料、换一个合法
// 数量、更换登记人、换一个投料时间，或改到另一个同样处于执行中的批次，
// 不靠非法输入被拒来代表请求冲突。内容不同的提交不能覆盖原投料、不能追加
// 新投料、不能占用下一个登记序号。
func TestAddFeedingRequestConflictOnAnyContentChange(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	// 另一个同样处于执行中的批次 B2，绑定同一配方版本，使“改批次”的
	// 提交本身也是一笔合法投料。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatalf("开始执行 B2 失败: %v", err)
	}

	t1 := feedingReplayTime()
	first, err := s.AddFeeding("feed-orig", "B1", "M1", "10", t1, "张三")
	if err != nil {
		t.Fatalf("首次登记投料应成功: %v", err)
	}
	checkFeedingView(t, first, 1, "M1", "10", t1, "张三")
	// 再追加一笔，占用序号 2，便于发现冲突提交是否偷偷追加或占号。
	if _, err := s.AddFeeding("feed-other", "B1", "M2", "2", t1.Add(time.Hour), "李四"); err != nil {
		t.Fatalf("追加第二笔投料失败: %v", err)
	}

	// 每项都只改一处，其余与首次成功提交完全一致，且改后内容仍可正常投料。
	cases := []struct {
		name      string
		batchNo   string
		material  string
		grams     string
		tm        time.Time
		registrar string
	}{
		{name: "改投同一配方中的另一种物料", batchNo: "B1", material: "M2", grams: "10", tm: t1, registrar: "张三"},
		{name: "换一个合法数量", batchNo: "B1", material: "M1", grams: "11", tm: t1, registrar: "张三"},
		{name: "更换投料时间", batchNo: "B1", material: "M1", grams: "10", tm: t1.Add(time.Minute), registrar: "张三"},
		{name: "更换登记人", batchNo: "B1", material: "M1", grams: "10", tm: t1, registrar: "李四"},
		{name: "改到另一个执行中的批次", batchNo: "B2", material: "M1", grams: "10", tm: t1, registrar: "张三"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AddFeeding("feed-orig", tc.batchNo, tc.material, tc.grams, tc.tm, tc.registrar)
			if !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("内容不同但沿用原请求编号应返回 ErrRequestConflict，得到 %v", err)
			}
		})

		// 每次冲突后 B1 都保持原样：仍是 2 条投料、顺序不变，
		// M1 累计仍为 10、M2 累计仍为 2，冲突内容没有被追加或覆盖。
		view := mustGetBatch(t, s, "B1")
		if len(view.Feedings) != 2 {
			t.Fatalf("%s：冲突不应追加投料，B1 应仍为 2 条，得到 %d 条: %+v",
				tc.name, len(view.Feedings), view.Feedings)
		}
		checkFeedingView(t, &view.Feedings[0], 1, "M1", "10", t1, "张三")
		checkFeedingView(t, &view.Feedings[1], 2, "M2", "2", t1.Add(time.Hour), "李四")
		b1Mats := materialsMap(view)
		checkRequirement(t, b1Mats, "M1", "1000", "10", "-990")
		checkRequirement(t, b1Mats, "M2", "5", "2", "-3")
		checkRequirement(t, b1Mats, "M3", "0.1", "0", "-0.1")

		// 下一个登记序号不能被冲突占用：B2 的“改批次”冲突同样不能落进 B2。
		b2 := mustGetBatch(t, s, "B2")
		if len(b2.Feedings) != 0 {
			t.Fatalf("%s：冲突不应在 B2 追加投料，得到 %d 条: %+v",
				tc.name, len(b2.Feedings), b2.Feedings)
		}
	}

	// 全部冲突之后，原内容用原编号仍取得第一次成功结果，不是最近一笔。
	replay, err := s.AddFeeding("feed-orig", "B1", "M1", "10", t1, "张三")
	if err != nil {
		t.Fatalf("冲突后原内容重放应成功: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "10", t1, "张三")
}

// 克数的显示格式与请求内容必须分清：首次提交“1.000”克，成功结果显示“1”克；
// 再次提交仍带“1.000”时能取回原结果；改成“1”（或“1.0”）时即使数值相等，
// 也属于不同内容，必须返回 ErrRequestConflict。冲突后原内容仍可取得原结果，
// 且批次中始终只有首次那一笔投料。
func TestAddFeedingGramsFormatIsPartOfRequestContent(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}

	t1 := feedingReplayTime()
	first, err := s.AddFeeding("feed-fmt", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("首次登记 1.000 克应成功: %v", err)
	}
	// 存储以千分之一克定点表示，成功结果去掉末尾多余的 0，显示为“1”。
	checkFeedingView(t, first, 1, "M1", "1", t1, "张三")

	// 仍带原字符串“1.000”重放：同一内容，取回原结果，不重复计入实投。
	replay, err := s.AddFeeding("feed-fmt", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("仍带 1.000 重放应返回原结果: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")

	// 改成数值相等但字符串不同的“1”：请求内容不同，必须判冲突，
	// 不能按数值相等当成同一笔投料重放。
	if _, err := s.AddFeeding("feed-fmt", "B1", "M1", "1", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("1.000 改成数值相等的 1 应返回 ErrRequestConflict，得到 %v", err)
	}
	// “1.0”同样只是显示格式不同、数值相等，也属于不同内容。
	if _, err := s.AddFeeding("feed-fmt", "B1", "M1", "1.0", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("1.000 改成数值相等的 1.0 应返回 ErrRequestConflict，得到 %v", err)
	}

	// 冲突之后，原字符串“1.000”仍能取得第一次的结果。
	replayAgain, err := s.AddFeeding("feed-fmt", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("冲突后原内容应仍可重放: %v", err)
	}
	checkFeedingView(t, replayAgain, 1, "M1", "1", t1, "张三")

	// 批次始终只有首次那一笔投料，M1 累计实投为 1，没有被数值相等的
	// 冲突提交重复计入。
	view := mustGetBatch(t, s, "B1")
	if len(view.Feedings) != 1 {
		t.Fatalf("冲突提交不应追加投料，应只有 1 条，得到 %d 条: %+v",
			len(view.Feedings), view.Feedings)
	}
	checkFeedingView(t, &view.Feedings[0], 1, "M1", "1", t1, "张三")
	mats := materialsMap(view)
	checkRequirement(t, mats, "M1", "1000", "1", "-999")
	checkRequirement(t, mats, "M2", "5", "0", "-5")
}

// 请求编号在同一台账内共用：另一个同样处于执行中的批次也不能沿用这个编号
// 登记投料。冲突提交不能进入任一批次，两个批次的已有记录都应保持原样；
// 原编号在原批次上仍取回第一次的结果，新批次只能用自己的新编号登记。
func TestAddFeedingRequestNumberSharedAcrossBatches(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatalf("开始执行 B2 失败: %v", err)
	}

	t1 := feedingReplayTime()
	first, err := s.AddFeeding("shared-feed", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("B1 首次登记投料应成功: %v", err)
	}
	checkFeedingView(t, first, 1, "M1", "1", t1, "张三")

	// B2 沿用同一请求编号登记一笔本身合法的投料：必须判冲突，
	// 不能在 B2 落下记录，也不能返回 B1 的那笔记录冒充成功。
	_, err = s.AddFeeding("shared-feed", "B2", "M1", "1.000", t1, "张三")
	if !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("另一批次沿用已成功的请求编号应返回 ErrRequestConflict，得到 %v", err)
	}

	// 两个批次的已有记录都保持原样。
	b1 := mustGetBatch(t, s, "B1")
	if len(b1.Feedings) != 1 {
		t.Fatalf("B1 应仍只有 1 条投料，得到 %d 条: %+v", len(b1.Feedings), b1.Feedings)
	}
	checkFeedingView(t, &b1.Feedings[0], 1, "M1", "1", t1, "张三")
	b1Mats := materialsMap(b1)
	checkRequirement(t, b1Mats, "M1", "1000", "1", "-999")

	b2 := mustGetBatch(t, s, "B2")
	if len(b2.Feedings) != 0 {
		t.Fatalf("冲突提交不应进入 B2，得到 %d 条: %+v", len(b2.Feedings), b2.Feedings)
	}
	b2Mats := materialsMap(b2)
	checkRequirement(t, b2Mats, "M1", "500", "0", "-500") // 100×5，无投料
	checkRequirement(t, b2Mats, "M2", "2.5", "0", "-2.5") // 0.5×5

	// 原编号在原批次上仍取回 B1 的第一次结果。
	replay, err := s.AddFeeding("shared-feed", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("冲突后原编号在原批次上应仍可重放: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")

	// B2 改用尚未使用的请求编号，可以正常登记自己的第一笔投料，
	// 序号在 B2 内从 1 开始，与 B1 互不影响。
	b2Feed, err := s.AddFeeding("b2-feed", "B2", "M1", "4", t1.Add(time.Hour), "李四")
	if err != nil {
		t.Fatalf("B2 用新编号登记投料应成功: %v", err)
	}
	checkFeedingView(t, b2Feed, 1, "M1", "4", t1.Add(time.Hour), "李四")
	b1 = mustGetBatch(t, s, "B1")
	if len(b1.Feedings) != 1 {
		t.Fatalf("B2 的投料不应影响 B1，B1 应仍为 1 条，得到 %d 条", len(b1.Feedings))
	}
}

// 发生冲突后，原内容仍能用原编号取得第一次成功结果；改用尚未使用的请求
// 编号提交完全相同的投料内容，则表示独立的一笔投料：应正常追加、取得连续
// 的下一个序号，并只增加该物料的累计实投量。原编号的重放结果必须始终是
// 第一次那笔，不能把新追加的同内容投料误当成原记录。
func TestAddFeedingNewRequestSameContentAppendsIndependentFeeding(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}

	t1 := feedingReplayTime()
	first, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("首次登记投料应成功: %v", err)
	}
	checkFeedingView(t, first, 1, "M1", "1", t1, "张三")

	// 一次本身合法的冲突提交（改投同配方内另一物料），确认它不落任何记录。
	if _, err := s.AddFeeding("feed-1", "B1", "M2", "1.000", t1, "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("沿用原编号改投另一物料应返回 ErrRequestConflict，得到 %v", err)
	}

	// 原内容用原编号仍取得第一次成功结果。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("冲突后原内容应仍可重放: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "1", t1, "张三")

	// 改用尚未使用的请求编号提交与首次完全相同的投料内容：这是独立的一笔，
	// 正常追加，取得连续的下一个序号 2（冲突没有占用序号）。
	independent, err := s.AddFeeding("feed-2", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("新请求编号提交相同内容应作为独立投料成功: %v", err)
	}
	checkFeedingView(t, independent, 2, "M1", "1", t1, "张三")

	// 批次现有两笔，登记顺序与各自内容清晰；只增加 M1 的实投（1+1=2），
	// M2、M3 仍为零。
	view := mustGetBatch(t, s, "B1")
	if len(view.Feedings) != 2 {
		t.Fatalf("应追加为 2 条投料，得到 %d 条: %+v", len(view.Feedings), view.Feedings)
	}
	checkFeedingView(t, &view.Feedings[0], 1, "M1", "1", t1, "张三")
	checkFeedingView(t, &view.Feedings[1], 2, "M1", "1", t1, "张三")
	mats := materialsMap(view)
	checkRequirement(t, mats, "M1", "1000", "2", "-998")
	checkRequirement(t, mats, "M2", "5", "0", "-5")
	checkRequirement(t, mats, "M3", "0.1", "0", "-0.1")

	// 新投料追加后，原编号的重放结果必须仍是第一次那笔（序号 1），
	// 不能把后来同内容的序号 2 误当成原记录。
	replayAgain, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("追加同内容投料后原编号重放应成功: %v", err)
	}
	checkFeedingView(t, replayAgain, 1, "M1", "1", t1, "张三")
	// 新编号重放的是它自己的第一次结果（序号 2），两者各自独立。
	replayNew, err := s.AddFeeding("feed-2", "B1", "M1", "1.000", t1, "张三")
	if err != nil {
		t.Fatalf("新编号重放应成功: %v", err)
	}
	checkFeedingView(t, replayNew, 2, "M1", "1", t1, "张三")

	// 两次重放都不重复计入：仍是 2 条、M1 累计 2。
	view = mustGetBatch(t, s, "B1")
	if len(view.Feedings) != 2 {
		t.Fatalf("重放不应新增投料，得到 %d 条", len(view.Feedings))
	}
	mats = materialsMap(view)
	checkRequirement(t, mats, "M1", "1000", "2", "-998")
}
