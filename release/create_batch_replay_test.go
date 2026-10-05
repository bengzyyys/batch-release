package release

import (
	"encoding/json"
	"errors"
	"testing"
)

// 本文件为“创建批次”（CreateBatch）补充幂等重放回归测试，锁定这样一条
// 时序下的行为：先用已登记的配方版本与正整数份数创建一个草稿批次，随后
// 正常调整这个批次的配方版本及份数，再用“原请求编号 + 原创建内容”重复提交。
//
// 重复提交必须：
//   - 返回第一次创建成功时的完整结果（创建时的配方编号、版本、名称、份数、
//     草稿状态、空投料列表与当时的数量核对），不能因为批次编号已经存在就
//     返回 ErrDuplicateBatch，也不能把原创建请求当作一次新的计划调整
//     （当前批次必须保留后来调整出的计划）；
//   - 历史结果的数量核对完全按创建时版本的物料与每份克数、创建时份数逐项
//     计算，实投为零、差额为应投量的负值；两个版本含相同物料但每份克数
//     不同、或各自含有另一版没有的物料时，不能混用后来的用量、不能漏掉
//     原版本物料，也不能带入新版本独有的物料；千分之一克用量精确计算，
//     显示遵守现有克数格式；
//   - 重复提交前后，查询同一批次都看到调整后的版本、名称、份数与核对，
//     台账始终只有这一个批次；
//   - 调用方改写首次创建或重复提交返回的配方信息、份数与物料核对项，
//     只能影响自己持有的结果：下一次重放仍返回原创建结果，查询仍返回
//     当前计划，两个结果的物料列表互不影响；
//   - 与真正的请求冲突区分开：沿用原请求编号却改变创建内容中的批次编号、
//     配方编号、版本号或份数，必须返回 ErrRequestConflict——即使改后的
//     内容恰好等于当前草稿计划，也不能接受为原请求；换一个未使用的请求
//     编号创建同编号批次，仍返回 ErrDuplicateBatch。两类拒绝都不得改动
//     当前批次或替换原请求的成功结果。
//
// 专用的两版配方（与 registerReplayRecipes 的数值刻意不同，便于本文件独立
// 核对千分之一克精度）：
//   - R1/v1（配方初版）：M1=0.125、M2=0.001（M2 仅 v1 有，且每份就是
//     最小的千分之一克）；
//   - R1/v2（配方改版）：M1=250（共同物料每份克数不同）、M4=2（M4 仅 v2 有）。
//
// 创建时用 R1/v1、8 份：M1 应投 0.125×8=1，M2 应投 0.001×8=0.008。
// 调整后用 R1/v2、3 份：M1 应投 250×3=750，M4 应投 2×3=6。
// 两个计划从物料集合与数量上都能明确区分。
func registerCreateReplayRecipes(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("c-recipe-v1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.125"},
		{MaterialNo: "M2", Grams: "0.001"},
	}); err != nil {
		t.Fatalf("登记 R1/v1 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("c-recipe-v2", "R1", "v2", "配方改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M4", Grams: "2"},
	}); err != nil {
		t.Fatalf("登记 R1/v2 失败: %v", err)
	}
}

// checkFirstCreateView 校验“创建 B1（R1/v1、8 份）”第一次成功时的完整结果。
// 批次后来被调整到 R1/v2、3 份后，重放原创建请求也必须返回这个历史结果：
//   - 配方编号、版本、名称与份数全部来自最初创建，不能混入后来调整的计划；
//   - 状态仍为草稿，投料列表仍为空；
//   - 数量核对按最初版本的物料顺序逐项列出：M1=0.125×8=1、M2=0.001×8=0.008，
//     实投为零，差额为应投量的负值；
//   - 不能出现新版本独有的物料 M4，M1 也不能套用后来的每份 250 克。
func checkFirstCreateView(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "R1" || v.RecipeVersion != "v1" || v.RecipeName != "配方初版" {
		t.Fatalf("重放应返回最初创建时的 R1/v1（配方初版），得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 8 {
		t.Fatalf("计划份数应为最初创建的 8，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusDraft {
		t.Fatalf("最初创建的结果应为草稿，得到 %s", v.Status)
	}
	if len(v.Feedings) != 0 {
		t.Fatalf("最初创建时投料列表应为空，得到 %d 条: %+v", len(v.Feedings), v.Feedings)
	}
	if len(v.Materials) != 2 {
		t.Fatalf("最初版本只有 M1、M2 两种物料，得到 %d 项: %+v", len(v.Materials), v.Materials)
	}
	// 物料顺序必须与最初版本一致，不能被后来版本的物料列表带跑。
	if v.Materials[0].MaterialNo != "M1" || v.Materials[1].MaterialNo != "M2" {
		t.Fatalf("物料顺序应与 R1/v1 一致（M1、M2），得到 %+v", v.Materials)
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "M1", "1", "0", "-1")       // 0.125 × 8，精确为 1
	checkRequirement(t, mats, "M2", "0.008", "0", "-0.008") // 0.001 × 8，千分之一克精度
	if _, leftover := mats["M4"]; leftover {
		t.Fatalf("重放创建结果不能带入新版本独有的物料 M4: %+v", mats["M4"])
	}
}

// checkAdjustedCurrent 校验 B1 调整为 R1/v2、3 份之后的“当前台账记录”：
// 查询在任何重放前后都必须保持这个结果，不能被历史创建结果回退。
func checkAdjustedCurrent(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" || v.RecipeNo != "R1" || v.RecipeVersion != "v2" ||
		v.RecipeName != "配方改版" || v.PlannedPortions != 3 || v.Status != StatusDraft {
		t.Fatalf("当前批次应保留调整后的计划（R1/v2、配方改版、3 份、草稿），得到 %+v", v)
	}
	if len(v.Feedings) != 0 {
		t.Fatalf("当前草稿不应有投料，得到 %d 条: %+v", len(v.Feedings), v.Feedings)
	}
	if len(v.Materials) != 2 {
		t.Fatalf("当前计划只应有 M1、M4 两种物料，得到 %d 项: %+v", len(v.Materials), v.Materials)
	}
	if v.Materials[0].MaterialNo != "M1" || v.Materials[1].MaterialNo != "M4" {
		t.Fatalf("物料顺序应与 R1/v2 一致（M1、M4），得到 %+v", v.Materials)
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "M1", "750", "0", "-750") // 250 × 3，不是创建时的 0.125 × 8
	checkRequirement(t, mats, "M4", "6", "0", "-6")     // 2 × 3
	if _, leftover := mats["M2"]; leftover {
		t.Fatalf("当前计划不应残留旧版本物料 M2: %+v", mats["M2"])
	}
}

// 创建草稿 → 正常调整配方版本与份数 → 用原请求编号和原创建内容重复提交：
// 每次重放都返回第一次创建成功时的完整结果（含按最初版本、最初份数的
// 数量核对），既不因批次编号已存在被拒绝，也不把当前计划回退成创建时的
// 内容；台账始终只有一个批次，原创建请求记录不被新增或替换。
// 重新打开台账后，重放与查询仍各自反映应有的内容。
func TestCreateBatchReplayAfterDraftAdjustReturnsFirstResult(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCreateReplayRecipes(t, s)

	first, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("首次创建批次应成功: %v", err)
	}
	checkFirstCreateView(t, first)

	// 随后正常调整这个批次的配方版本及份数。
	adj, err := s.UpdateDraftBatch("adjust-b1", "B1", "R1", "v2", 3)
	if err != nil {
		t.Fatalf("调整草稿应成功: %v", err)
	}
	if adj.RecipeNo != "R1" || adj.RecipeVersion != "v2" || adj.PlannedPortions != 3 ||
		adj.Status != StatusDraft {
		t.Fatalf("调整结果应为 R1/v2、3 份的草稿，得到 %+v", adj)
	}

	// 重复提交前，查询看到的就是调整后的计划。
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 用原请求编号与原创建内容重复提交：必须返回第一次创建成功的完整结果，
	// 不能返回 ErrDuplicateBatch（由 err == nil 直接保证），
	// 也不能被当成一次新的计划调整。
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("草稿调整后重放原创建请求应成功返回首次结果: %v", err)
	}
	checkFirstCreateView(t, replay)
	// 重放之后查询：当前批次仍保留后来调整的计划，没有被回退。
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 再查询与再重放交替进行：历史结果始终是创建时的结果，查询始终是当前计划。
	replay2, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("再次重放原创建请求应成功: %v", err)
	}
	checkFirstCreateView(t, replay2)
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 白盒核对：多次重放后台账始终只有一个批次；原创建请求记录仍在，
	// 操作类型、载荷与保存的结果都还是第一次创建时的内容，没有被重放
	// 新增或替换成调整后的结果。
	if err := s.read(func(st *persistedState) error {
		if len(st.Batches) != 1 {
			t.Fatalf("重放不应新增批次，应始终只有 1 个，得到 %d 个", len(st.Batches))
		}
		req := st.Requests["create-b1"]
		if req == nil {
			t.Fatal("原创建请求记录丢失")
		}
		if req.Op != opCreateBatch {
			t.Fatalf("原创建请求的操作类型被改写，得到 %q", req.Op)
		}
		var payload createBatchPayload
		if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
			t.Fatalf("原创建请求载荷无法解析: %v", err)
		}
		if payload != (createBatchPayload{BatchNo: "B1", RecipeNo: "R1", Version: "v1", Portions: 8}) {
			t.Fatalf("原创建请求载荷被改写，得到 %+v", payload)
		}
		var saved BatchView
		if err := json.Unmarshal(req.Result, &saved); err != nil {
			t.Fatalf("原创建请求保存的结果无法解析: %v", err)
		}
		if saved.RecipeNo != "R1" || saved.RecipeVersion != "v1" ||
			saved.RecipeName != "配方初版" || saved.PlannedPortions != 8 ||
			saved.Status != StatusDraft {
			t.Fatalf("保存的创建结果被后来的调整污染: %+v", saved)
		}
		return nil
	}); err != nil {
		t.Fatalf("读取台账内部状态失败: %v", err)
	}

	// 重新打开台账后：重放仍返回第一次创建的结果，查询仍显示调整后的计划。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	replayAfterReopen, err := s2.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重新打开后重放原创建请求应成功: %v", err)
	}
	checkFirstCreateView(t, replayAfterReopen)
	checkAdjustedCurrent(t, mustGetBatch(t, s2, "B1"))
}

// 调用方改写首次创建返回对象或重放返回对象中的配方信息、份数、状态、
// 投料列表与物料核对项，只能影响它自己持有的结果：下一次重放仍返回原
// 创建结果，查询仍返回当前调整后的计划，两个结果的物料列表不能相互影响。
func TestCreateBatchReplayResultIsCallerOwnedCopy(t *testing.T) {
	s := openTestStore(t)
	registerCreateReplayRecipes(t, s)

	first, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("首次创建批次应成功: %v", err)
	}
	checkFirstCreateView(t, first)
	if _, err := s.UpdateDraftBatch("adjust-b1", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("调整草稿应成功: %v", err)
	}

	// 污染调用方持有的首次返回对象：配方信息、份数、状态、投料、物料核对，
	// 再追加伪造项。
	first.RecipeNo = "RX"
	first.RecipeVersion = "v9"
	first.RecipeName = "被污染的名称"
	first.PlannedPortions = 99
	first.Status = StatusClosed
	first.Feedings = append(first.Feedings, FeedingView{
		Seq: 1, MaterialNo: "FAKE", Grams: "999", Registrar: "外人",
	})
	first.Materials[0].RequiredGrams = "1"
	first.Materials[0].ActualGrams = "999"
	first.Materials[0].DifferenceGrams = "998"
	first.Materials = append(first.Materials, MaterialRequirement{MaterialNo: "FAKE"})

	// 再提交同一创建请求：重放结果必须仍是干净的首次创建结果。
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("污染首次返回对象后重放应成功: %v", err)
	}
	checkFirstCreateView(t, replay)
	// 查询不受污染：仍是调整后的当前计划。
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 再污染重放对象，第二轮重放与查询仍各自干净，两份结果的物料列表互不相干。
	replay.Status = StatusClosed
	replay.PlannedPortions = 77
	replay.Materials[0].ActualGrams = "42"
	replay.Feedings = append(replay.Feedings, FeedingView{Seq: 2, MaterialNo: "FAKE2"})
	replay2, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("污染重放对象后再次重放应成功: %v", err)
	}
	checkFirstCreateView(t, replay2)
	// 上一份重放对象的污染不能进入这一份结果。
	if replay2.Materials[0].ActualGrams != "0" || len(replay2.Feedings) != 0 {
		t.Fatalf("两次重放结果的物料列表相互影响了: %+v", replay2)
	}
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))
}

// 区分重复提交与请求冲突、批次重复：
//   - 沿用原创建请求编号、却改变创建内容中的批次编号、配方编号、版本号或
//     份数，一律返回 ErrRequestConflict；即使改后的内容恰好等于当前草稿
//     计划，也不能接受为原请求。每个改动本身都是合法的创建内容，不靠
//     非法输入被拒来冒充冲突；
//   - 换一个未使用的请求编号创建同编号批次，仍返回 ErrDuplicateBatch
//     （即使内容与当前草稿计划完全一致）；
//   - 两类拒绝都不改动当前批次、不创建旁支批次，也不替换原请求的成功结果；
//   - 被拒绝的新编号没有被失败占用，修正为创建尚不存在的批次后仍可成功。
func TestCreateBatchReplayDistinguishesConflictAndDuplicate(t *testing.T) {
	s := openTestStore(t)
	registerCreateReplayRecipes(t, s)
	// 另一个真实存在的配方版本，供“改配方编号”场景使用，使改后内容本身合法。
	if _, err := s.RegisterRecipe("c-recipe-r2", "R2", "v1", "另一配方", []MaterialInput{
		{MaterialNo: "M5", Grams: "1"},
	}); err != nil {
		t.Fatalf("登记 R2/v1 失败: %v", err)
	}

	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8); err != nil {
		t.Fatalf("首次创建批次应成功: %v", err)
	}
	if _, err := s.UpdateDraftBatch("adjust-b1", "B1", "R1", "v2", 3); err != nil {
		t.Fatalf("调整草稿应成功: %v", err)
	}
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 沿用原请求编号、只改创建内容中的一项，全部必须判 ErrRequestConflict。
	conflictCases := []struct {
		name     string
		batchNo  string
		recipeNo string
		version  string
		portions int
	}{
		// 改批次编号：B2 尚不存在，若无请求记录这本是一次合法创建。
		{"改批次编号", "B2", "R1", "v1", 8},
		// 改配方编号：R2/v1 已登记；若无请求记录这本会因 B1 已存在而得到
		// ErrDuplicateBatch——请求冲突必须先于批次重复判定。
		{"改配方编号", "B1", "R2", "v1", 8},
		// 改版本号：R1/v2 已登记，份数沿用，改后是“当前版本 + 旧份数”。
		{"改版本号", "B1", "R1", "v2", 8},
		// 改份数：版本沿用，改后是“旧版本 + 当前份数”。
		{"改份数", "B1", "R1", "v1", 3},
		// 批次、版本、份数恰好等于当前草稿调整后的计划：也不能接受为原请求。
		{"改后内容恰好等于当前草稿计划", "B1", "R1", "v2", 3},
	}
	for _, tc := range conflictCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateBatch("create-b1", tc.batchNo, tc.recipeNo, tc.version, tc.portions)
			if !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("沿用原请求编号改变创建内容（%s）应返回 ErrRequestConflict，得到 %v",
					tc.name, err)
			}
		})
	}

	// 冲突提交不能创建旁支批次：B2 始终不存在。
	if _, err := s.GetBatch("B2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("冲突提交不应创建 B2，查询应返回 ErrNotFound，得到 %v", err)
	}
	// 冲突提交不能改动当前批次：仍是调整后的 R1/v2、3 份草稿。
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 换用未使用的请求编号创建同编号批次：不是重放，批次编号已存在，
	// 必须返回 ErrDuplicateBatch——即使提交内容与当前草稿计划完全一致。
	if _, err := s.CreateBatch("create-fresh", "B1", "R1", "v1", 8); !errors.Is(err, ErrDuplicateBatch) {
		t.Fatalf("新请求编号创建已存在批次应返回 ErrDuplicateBatch，得到 %v", err)
	}
	if _, err := s.CreateBatch("create-fresh-same", "B1", "R1", "v2", 3); !errors.Is(err, ErrDuplicateBatch) {
		t.Fatalf("内容与当前计划一致的新请求仍应返回 ErrDuplicateBatch，得到 %v", err)
	}

	// 两类拒绝都不改动当前批次。
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 原创建请求的成功结果不被任何拒绝替换：仍可取回最初创建的完整结果。
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("拒绝后原创建请求仍应可重放: %v", err)
	}
	checkFirstCreateView(t, replay)
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))

	// 业务失败不占用新请求编号：duplicate 失败后，同一编号改用于创建尚不
	// 存在的 B3（合法内容）应成功，且 B3 是独立草稿，不影响 B1 的当前计划。
	b3, err := s.CreateBatch("create-fresh", "B3", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("失败不应占用请求编号，改创建 B3 应成功: %v", err)
	}
	if b3.BatchNo != "B3" || b3.RecipeVersion != "v1" || b3.PlannedPortions != 8 ||
		b3.Status != StatusDraft || len(b3.Feedings) != 0 {
		t.Fatalf("B3 应为独立的 R1/v1、8 份草稿，得到 %+v", b3)
	}
	b3Mats := materialsMap(b3)
	if len(b3Mats) != 2 {
		t.Fatalf("B3 应按 R1/v1 列出 2 种物料，得到 %d 项: %+v", len(b3Mats), b3.Materials)
	}
	checkRequirement(t, b3Mats, "M1", "1", "0", "-1")
	checkRequirement(t, b3Mats, "M2", "0.008", "0", "-0.008")
	if _, has := b3Mats["M4"]; has {
		t.Fatalf("B3 使用 v1，不应带入 v2 独有的 M4: %+v", b3Mats["M4"])
	}
	// B1 的当前计划不受 B3 创建影响。
	checkAdjustedCurrent(t, mustGetBatch(t, s, "B1"))
}
