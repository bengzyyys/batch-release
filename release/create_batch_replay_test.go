package release

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// countBatchesOnDisk 直接读取持久化文件统计批次记录总数，
// 用来确认重放与各类拒绝都没有在文件里新增第二条批次。
func countBatchesOnDisk(t *testing.T, s *Store) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, stateFileName))
	if err != nil {
		t.Fatalf("读取台账文件失败: %v", err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("解析台账文件失败: %v", err)
	}
	return len(st.Batches)
}

// 创建批次重放回归专用的两版配方，刻意让“最初创建”与“后来调整”的
// 数量核对在物料集合与每项数值上都能明确区分：
//   - RA/vA（创建时采用）：MX=0.004（千分之一克精度也要精确乘份数）、
//     MO=1.5（MO 仅 vA 含有）；
//   - RA/vB（调整后采用）：MX=0.25（同一物料每份克数不同）、
//     MN=0.002（MN 仅 vB 含有）。
//
// 共同物料 MX 的每份克数两版相差 62.5 倍且都带三位小数；各自又有独有
// 物料，因此历史创建结果一旦混入后来用量、漏掉旧物料或带入新物料，
// 断言立刻可见。
func registerCreateReplayRecipes(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("ra-va", "RA", "vA", "配方甲版", []MaterialInput{
		{MaterialNo: "MX", Grams: "0.004"},
		{MaterialNo: "MO", Grams: "1.5"},
	}); err != nil {
		t.Fatalf("登记 RA/vA 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("ra-vb", "RA", "vB", "配方乙版", []MaterialInput{
		{MaterialNo: "MX", Grams: "0.25"},
		{MaterialNo: "MN", Grams: "0.002"},
	}); err != nil {
		t.Fatalf("登记 RA/vB 失败: %v", err)
	}
}

// checkOriginalCreateView 断言视图就是 B1 第一次创建成功时的完整结果：
// RA/vA、4 份、草稿、无投料；数量核对按 vA 的物料与每份克数、4 份逐项
// 计算——MX = 0.004 × 4 = 0.016，MO = 1.5 × 4 = 6，实投为零，
// 差额为应投量的负值；不能出现 vB 独有的 MN，也不能套用 vB 的 0.25。
func checkOriginalCreateView(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("重放结果批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "RA" || v.RecipeVersion != "vA" || v.RecipeName != "配方甲版" {
		t.Fatalf("重放结果应保留创建时的 RA/vA/配方甲版，得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 4 {
		t.Fatalf("重放结果的计划份数应为创建时的 4，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusDraft {
		t.Fatalf("重放结果状态应为创建时的草稿，得到 %s", v.Status)
	}
	if len(v.Feedings) != 0 {
		t.Fatalf("草稿重放结果的投料列表必须为空，得到 %d 条", len(v.Feedings))
	}
	// 物料项目顺序与数量都按 vA 原样保留：先 MX 后 MO。
	if len(v.Materials) != 2 {
		t.Fatalf("重放结果应逐项列出 vA 的 2 种物料，得到 %d 项: %+v",
			len(v.Materials), v.Materials)
	}
	if v.Materials[0].MaterialNo != "MX" || v.Materials[1].MaterialNo != "MO" {
		t.Fatalf("重放结果物料顺序应与创建时配方一致（MX、MO），得到 %+v", v.Materials)
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "MX", "0.016", "0", "-0.016") // 0.004 × 4
	checkRequirement(t, mats, "MO", "6", "0", "-6")         // 1.5 × 4
	if _, mixed := mats["MN"]; mixed {
		t.Fatalf("重放结果不能带入调整后版本独有的物料 MN: %+v", mats["MN"])
	}
}

// checkCurrentAdjustedPlan 断言台账中 B1 当前仍是调整后的计划：
// RA/vB、7 份、草稿，数量核对按 vB 计算——MX = 0.25 × 7 = 1.75，
// MN = 0.002 × 7 = 0.014；不能残留 vA 独有的 MO，也不能回退成创建时
// 的 0.004、1.5 与 4 份。
func checkCurrentAdjustedPlan(t *testing.T, s *Store) {
	t.Helper()
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询当前批次失败: %v", err)
	}
	if got.BatchNo != "B1" || got.RecipeNo != "RA" || got.RecipeVersion != "vB" ||
		got.RecipeName != "配方乙版" || got.PlannedPortions != 7 || got.Status != StatusDraft {
		t.Fatalf("当前批次应保留调整后的计划（RA/vB、7 份、草稿），得到 %+v", got)
	}
	if len(got.Feedings) != 0 {
		t.Fatalf("当前草稿不应有投料，得到 %d 条", len(got.Feedings))
	}
	mats := materialsMap(got)
	if len(mats) != 2 {
		t.Fatalf("当前计划应逐项列出 vB 的 2 种物料，得到 %d 项: %+v", len(mats), mats)
	}
	checkRequirement(t, mats, "MX", "1.75", "0", "-1.75")   // 0.25 × 7
	checkRequirement(t, mats, "MN", "0.014", "0", "-0.014") // 0.002 × 7
	if _, leftover := mats["MO"]; leftover {
		t.Fatalf("当前计划不应残留创建时版本独有的物料 MO: %+v", mats["MO"])
	}
}

// 核心回归：先以已登记配方版本和正整数份数创建草稿批次，随后正常调整
// 该批次的配方版本与份数，再用“原请求编号 + 原创建内容”重复提交。
// 此时返回的必须是第一次创建成功时的完整结果，而不能因为批次编号已
// 存在返回 ErrDuplicateBatch，也不能把原创建请求当作一次新的计划调整
// （把当前计划改回创建时内容）；重复提交前后查询同一批次都应看到
// 调整后的计划，台账始终只有一个批次。
func TestCreateBatchReplayAfterDraftAdjustReturnsOriginalResult(t *testing.T) {
	s := openTestStore(t)
	registerCreateReplayRecipes(t, s)

	first, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("创建草稿批次失败: %v", err)
	}
	checkOriginalCreateView(t, first)

	// 正常调整：改选配方版本（共同物料每份克数不同、独有物料互换）并改份数。
	adjusted, err := s.UpdateDraftBatch("u-adjust", "B1", "RA", "vB", 7)
	if err != nil {
		t.Fatalf("调整草稿计划应成功: %v", err)
	}
	if adjusted.RecipeVersion != "vB" || adjusted.PlannedPortions != 7 {
		t.Fatalf("调整结果应指向 RA/vB、7 份，得到 %+v", adjusted)
	}

	// 调整生效后、重放前，查询看到的就是调整后的计划。
	checkCurrentAdjustedPlan(t, s)

	// 原请求编号、原创建内容重复提交：成功返回第一次创建时的完整结果，
	// 不能报批次编号重复，也不能把当前计划改回创建时内容。
	replay, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("草稿调整后用原请求编号与原内容重复提交，应返回第一次创建结果，得到 %v", err)
	}
	checkOriginalCreateView(t, replay)

	// 再重复提交一次，结果仍稳定为创建时结果。
	replay2, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("再次重复提交应仍返回第一次创建结果: %v", err)
	}
	checkOriginalCreateView(t, replay2)

	// 重放后查询：当前批次仍保留调整后的计划，没有被历史结果回退。
	checkCurrentAdjustedPlan(t, s)

	// 台账始终只有一个批次，重放没有新增第二条 B1 记录。
	if count := countBatchesOnDisk(t, s); count != 1 {
		t.Fatalf("重复提交不应新增批次，台账应有 1 个批次，得到 %d", count)
	}
}

// 调用方改写首次创建结果与重放结果里的配方信息、份数和物料核对项，
// 只能影响自己持有的副本：下一次重放仍返回原创建结果，查询仍返回当前
// 计划，历史结果与当前计划的物料列表互不影响。
func TestCreateBatchReplayReturnedViewsAreIndependentCopies(t *testing.T) {
	s := openTestStore(t)
	registerCreateReplayRecipes(t, s)

	first, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("创建草稿批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-adjust", "B1", "RA", "vB", 7); err != nil {
		t.Fatalf("调整草稿计划应成功: %v", err)
	}

	// 改写第一次创建返回的副本：配方信息、份数、状态、投料与物料核对项。
	first.RecipeNo = "TAMPERED"
	first.RecipeVersion = "vZ"
	first.RecipeName = "被篡改"
	first.PlannedPortions = 999
	first.Status = StatusClosed
	first.Feedings = append(first.Feedings, FeedingView{Seq: 1, MaterialNo: "MX", Grams: "9"})
	for i := range first.Materials {
		first.Materials[i].RequiredGrams = "999"
		first.Materials[i].ActualGrams = "888"
		first.Materials[i].DifferenceGrams = "777"
	}

	replay, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("改写历史返回后重放应仍成功: %v", err)
	}
	checkOriginalCreateView(t, replay)

	// 再改写重放返回的副本，历史结果与当前计划都不受影响。
	replay.Materials[0].MaterialNo = "HACK"
	replay.Materials[0].RequiredGrams = "0"
	replay.PlannedPortions = -3
	replay.Feedings = append(replay.Feedings, FeedingView{Seq: 1, MaterialNo: "MN", Grams: "0.001"})

	replay2, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("再次重放应仍返回原创建结果: %v", err)
	}
	checkOriginalCreateView(t, replay2)

	// 查询仍是调整后的当前计划，物料列表不被任何篡改副本污染。
	checkCurrentAdjustedPlan(t, s)
}

// 关闭重开台账后：原创建请求的成功结果仍可原样重放，当前批次仍保留
// 调整后的计划，持久化文件中的批次只有一个。
func TestCreateBatchReplayPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCreateReplayRecipes(t, s)
	if _, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4); err != nil {
		t.Fatalf("创建草稿批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-adjust", "B1", "RA", "vB", 7); err != nil {
		t.Fatalf("调整草稿计划应成功: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	// 重开后重放原创建请求：返回的仍是创建时 RA/vA、4 份的完整结果。
	replay, err := s2.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("重开后重放原创建请求应成功: %v", err)
	}
	checkOriginalCreateView(t, replay)

	// 当前批次仍是调整后的 RA/vB、7 份，没有被重放回退。
	checkCurrentAdjustedPlan(t, s2)

	if count := countBatchesOnDisk(t, s2); count != 1 {
		t.Fatalf("重开后重放不应新增批次，台账应有 1 个批次，得到 %d", count)
	}
}

// 必须区分“同一请求的重复提交”与“请求冲突/普通重复创建”：
//   - 沿用原创建请求编号，却改动创建内容中的批次编号、配方编号、版本号
//     或份数，一律 ErrRequestConflict——即使改后的内容恰好等于当前草稿
//     计划（RA/vB、7 份），也不能接受为原请求；
//   - 换一个未使用的请求编号创建同编号批次，仍是 ErrDuplicateBatch。
//
// 两类拒绝都不得改动当前批次，也不得替换原请求保存的成功结果。
func TestCreateBatchReplayDistinguishesConflictAndDuplicate(t *testing.T) {
	s := openTestStore(t)
	registerCreateReplayRecipes(t, s)
	if _, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4); err != nil {
		t.Fatalf("创建草稿批次失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-adjust", "B1", "RA", "vB", 7); err != nil {
		t.Fatalf("调整草稿计划应成功: %v", err)
	}

	// 沿用原请求编号、改批次编号：冲突。
	if _, err := s.CreateBatch("c-create", "B2", "RA", "vA", 4); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号改批次编号应返回 ErrRequestConflict，得到 %v", err)
	}
	// 沿用原请求编号、改配方编号：冲突。
	if _, err := s.CreateBatch("c-create", "B1", "RX", "vA", 4); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号改配方编号应返回 ErrRequestConflict，得到 %v", err)
	}
	// 沿用原请求编号、改版本号：冲突。
	if _, err := s.CreateBatch("c-create", "B1", "RA", "vB", 4); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号改版本号应返回 ErrRequestConflict，得到 %v", err)
	}
	// 沿用原请求编号、改份数：冲突。
	if _, err := s.CreateBatch("c-create", "B1", "RA", "vA", 5); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号改份数应返回 ErrRequestConflict，得到 %v", err)
	}
	// 改后的内容恰好等于当前草稿计划（RA/vB、7 份），仍因请求内容不同
	// 而冲突，绝不能因为“与当前计划一致”就接受这次提交或当成新的调整。
	if _, err := s.CreateBatch("c-create", "B1", "RA", "vB", 7); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号内容恰好等于当前计划也应返回 ErrRequestConflict，得到 %v", err)
	}

	// 换一个未使用的请求编号、创建同编号批次：普通的批次编号重复。
	_, dupErr := s.CreateBatch("c-fresh", "B1", "RA", "vB", 7)
	if !errors.Is(dupErr, ErrDuplicateBatch) {
		t.Fatalf("新请求编号创建同编号批次应返回 ErrDuplicateBatch，得到 %v", dupErr)
	}
	if errors.Is(dupErr, ErrRequestConflict) {
		t.Fatalf("未使用过的请求编号不应被判为请求冲突")
	}
	// 被拒绝的新编号没有被占用：换个不存在的批次号仍可正常创建。
	if _, err := s.CreateBatch("c-fresh", "B2", "RA", "vA", 1); err != nil {
		t.Fatalf("被 ErrDuplicateBatch 拒绝不应占用请求编号，得到 %v", err)
	}

	// 所有拒绝都没有改动 B1：仍是调整后的 RA/vB、7 份、草稿。
	checkCurrentAdjustedPlan(t, s)

	// 原创建请求保存的成功结果没有被任何冲突提交替换。
	replay, err := s.CreateBatch("c-create", "B1", "RA", "vA", 4)
	if err != nil {
		t.Fatalf("拒绝后原创建请求仍应可重放并返回原结果: %v", err)
	}
	checkOriginalCreateView(t, replay)

	// 只新增了合法创建的 B2，B1 仍唯一。
	if count := countBatchesOnDisk(t, s); count != 2 {
		t.Fatalf("拒绝冲突/重复不应新增记录，台账应有 B1、B2 共 2 个批次，得到 %d", count)
	}
}
