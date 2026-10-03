package release

import (
	"errors"
	"testing"
)

// 组合调整的两版配方：
//   - R1/v1：M1=100、M2=0.5、M3=0.010（M2 仅在 v1 中）
//   - R1/v2：M1=250（同一物料每份克数不同）、M4=2（M4 是新版本新增物料）
func registerTwoRecipeVersions(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.RegisterRecipe("recipe-v1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
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

func materialsMap(v *BatchView) map[string]MaterialRequirement {
	m := make(map[string]MaterialRequirement, len(v.Materials))
	for _, item := range v.Materials {
		m[item.MaterialNo] = item
	}
	return m
}

func checkRequirement(t *testing.T, got map[string]MaterialRequirement, mat, req, act, diff string) {
	t.Helper()
	m, ok := got[mat]
	if !ok {
		t.Fatalf("数量核对缺少物料 %q，实际有 %+v", mat, got)
	}
	if m.RequiredGrams != req || m.ActualGrams != act || m.DifferenceGrams != diff {
		t.Fatalf("物料 %q 核对应为 应投=%s 实投=%s 差额=%s，得到 应投=%s 实投=%s 差额=%s",
			mat, req, act, diff, m.RequiredGrams, m.ActualGrams, m.DifferenceGrams)
	}
}

// 一次提交同时调整配方版本与计划份数：两项调整一起生效。
// 数量核对必须完全采用新版本的每份克数与新份数：
//   - 同一物料 M1 在两个版本中每份克数不同（100 → 250），按新值计算；
//   - 新版本没有的旧物料 M2、M3 不残留；
//   - 新版本新增的 M4 即使从未投料也必须列出；
//   - 草稿累计实投为零，差额为应投量的负值；
//   - 批次编号不变，状态仍为草稿，返回结果与随后查询指向同一新结果。
func TestUpdateDraftRecipeAndPortionsAtomicSuccess(t *testing.T) {
	s := openTestStore(t)
	registerTwoRecipeVersions(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 调整前先记录原始数量核对，失败时用于比对回滚。
	before, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	beforeMats := materialsMap(before)

	view, err := s.UpdateDraftBatch("u-combo", "B1", "R1", "v2", 3)
	if err != nil {
		t.Fatalf("同时改配方与份数应成功: %v", err)
	}

	// 返回结果指向新版本、新份数，批次编号与状态不变。
	if view.BatchNo != "B1" {
		t.Fatalf("批次编号不应改变，得到 %q", view.BatchNo)
	}
	if view.RecipeNo != "R1" || view.RecipeVersion != "v2" || view.RecipeName != "配方改版" {
		t.Fatalf("应指向新配方版本 R1/v2，得到 %q %q %q", view.RecipeNo, view.RecipeVersion, view.RecipeName)
	}
	if view.PlannedPortions != 3 {
		t.Fatalf("计划份数应为 3，得到 %d", view.PlannedPortions)
	}
	if view.Status != StatusDraft {
		t.Fatalf("调整后仍应为草稿，得到 %s", view.Status)
	}
	if len(view.Feedings) != 0 {
		t.Fatalf("草稿不应有投料，得到 %d 条", len(view.Feedings))
	}

	// 数量核对完全按新版本物料 × 新份数：
	// M1 = 250 × 3 = 750（不是旧的 100 × 3 或 250 × 10）；M4 = 2 × 3 = 6。
	got := materialsMap(view)
	if len(got) != 2 {
		t.Fatalf("新版本只有 M1、M4 两种物料，得到 %d 项: %+v", len(got), view.Materials)
	}
	checkRequirement(t, got, "M1", "750", "0", "-750")
	checkRequirement(t, got, "M4", "6", "0", "-6")
	if _, leftover := got["M2"]; leftover {
		t.Fatalf("新版本没有的旧物料 M2 不应残留: %+v", got["M2"])
	}
	if _, leftover := got["M3"]; leftover {
		t.Fatalf("新版本没有的旧物料 M3 不应残留: %+v", got["M3"])
	}

	// 随后查询与返回结果一致。
	again, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("调整后查询失败: %v", err)
	}
	if again.BatchNo != "B1" || again.RecipeNo != "R1" || again.RecipeVersion != "v2" ||
		again.PlannedPortions != 3 || again.Status != StatusDraft {
		t.Fatalf("查询结果与调整返回不一致: %+v", again)
	}
	againMats := materialsMap(again)
	if len(againMats) != 2 {
		t.Fatalf("查询应只含新版本两种物料，得到 %d 项", len(againMats))
	}
	checkRequirement(t, againMats, "M1", "750", "0", "-750")
	checkRequirement(t, againMats, "M4", "6", "0", "-6")

	// 两个已登记的配方版本本身都不能被这次调整改写。
	r1, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("查询 R1/v1 失败: %v", err)
	}
	if r1.Name != "配方初版" || len(r1.Materials) != 3 {
		t.Fatalf("R1/v1 被调整改写: %+v", r1)
	}
	r1m := map[string]string{}
	for _, m := range r1.Materials {
		r1m[m.MaterialNo] = m.Grams
	}
	if r1m["M1"] != "100" || r1m["M2"] != "0.5" || r1m["M3"] != "0.01" {
		t.Fatalf("R1/v1 物料被改写: %+v", r1m)
	}
	r2, err := s.GetRecipe("R1", "v2")
	if err != nil {
		t.Fatalf("查询 R1/v2 失败: %v", err)
	}
	if r2.Name != "配方改版" || len(r2.Materials) != 2 {
		t.Fatalf("R1/v2 不应被改写: %+v", r2)
	}
	r2m := map[string]string{}
	for _, m := range r2.Materials {
		r2m[m.MaterialNo] = m.Grams
	}
	if r2m["M1"] != "250" || r2m["M4"] != "2" {
		t.Fatalf("R1/v2 物料被改写: %+v", r2m)
	}

	// 防止以后误把 before 优化掉：确认调整前核对确实不同。
	checkRequirement(t, beforeMats, "M1", "1000", "0", "-1000")
}

// 份数有效但选中的配方版本不存在：返回 ErrNotFound，
// 整批保留调整前的完整结果——原份数、原配方、原有数量核对一项都不能变。
func TestUpdateDraftComboMissingRecipeKeepsWholeBatch(t *testing.T) {
	s := openTestStore(t)
	registerTwoRecipeVersions(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	_, err := s.UpdateDraftBatch("u-missing", "B1", "R1", "v9", 5)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的配方版本应返回 ErrNotFound，得到 %v", err)
	}

	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("失败后查询批次失败: %v", err)
	}
	if got.BatchNo != "B1" || got.RecipeNo != "R1" || got.RecipeVersion != "v1" ||
		got.PlannedPortions != 10 || got.Status != StatusDraft {
		t.Fatalf("失败后应保留原批次（R1/v1、10 份、草稿），得到 %+v", got)
	}
	// 原数量核对仍完整：M1=100×10、M2=0.5×10、M3=0.010×10，三项都在。
	mats := materialsMap(got)
	if len(mats) != 3 {
		t.Fatalf("应保留原配方的全部 3 种物料，得到 %d 项: %+v", len(mats), mats)
	}
	checkRequirement(t, mats, "M1", "1000", "0", "-1000")
	checkRequirement(t, mats, "M2", "5", "0", "-5")
	checkRequirement(t, mats, "M3", "0.1", "0", "-0.1")
}

// 目标版本存在，但按新份数计算后某物料应投量超出可表示范围：
// 操作必须整体失败，不能留下已改好的份数或配方，不能出现整数回绕后的数量，
// 也不能只保留部分物料。判定按物料分别进行，不与其他物料相加。
func TestUpdateDraftComboRequiredOverflowRejectedAtomically(t *testing.T) {
	s := openTestStore(t)
	// v-ov：每份需要恰好上限 9223372036854775.807 克，另有一物料 M2。
	if _, err := s.RegisterRecipe("recipe-ov", "R9", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: "9223372036854775.807"},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatalf("登记上限配方失败: %v", err)
	}
	if _, err := s.RegisterRecipe("recipe-base", "R1", "v1", "基础配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatalf("登记基础配方失败: %v", err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 一份可以计算（恰好上限）：切版本同时改为 1 份应成功。
	one, err := s.UpdateDraftBatch("u-one", "B1", "R9", "v1", 1)
	if err != nil {
		t.Fatalf("应投恰好等于上限的一份应可计算: %v", err)
	}
	oneMats := materialsMap(one)
	checkRequirement(t, oneMats, "Mbig", "9223372036854775.807", "0", "-9223372036854775.807")
	checkRequirement(t, oneMats, "M2", "1", "0", "-1")

	// 同一提交再改为两份：Mbig 应投 = 上限 × 2 溢出，必须拒绝，
	// 不能把 Mbig 与 M2 的数量相加后再判断。
	_, err = s.UpdateDraftBatch("u-two", "B1", "R9", "v1", 2)
	if err == nil {
		t.Fatal("应投量溢出必须返回错误，却成功了")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("目标版本确实存在，不应返回 ErrNotFound，得到 %v", err)
	}

	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("失败后查询批次失败: %v", err)
	}
	// 批次保持在上一次成功状态（R9/v1、1 份、草稿），而不是半改成 2 份。
	if got.RecipeNo != "R9" || got.RecipeVersion != "v1" || got.PlannedPortions != 1 || got.Status != StatusDraft {
		t.Fatalf("溢出失败不应改动份数或配方，得到 配方=%q/%q 份数=%d 状态=%s",
			got.RecipeNo, got.RecipeVersion, got.PlannedPortions, got.Status)
	}
	// 数量核对仍为一份的完整结果：两种物料都在，Mbig 是上限原值而非回绕后的数。
	mats := materialsMap(got)
	if len(mats) != 2 {
		t.Fatalf("不能只保留部分物料，应仍有 2 项，得到 %d 项: %+v", len(mats), mats)
	}
	checkRequirement(t, mats, "Mbig", "9223372036854775.807", "0", "-9223372036854775.807")
	checkRequirement(t, mats, "M2", "1", "0", "-1")

	// 从原基础批次直接切到上限配方 × 2 份，同样必须整体回滚。
	if _, err := s.UpdateDraftBatch("u-reset", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("切回基础配方应成功: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-over", "B1", "R9", "v1", 2); err == nil {
		t.Fatal("切到上限配方并改为 2 份应失败，却成功了")
	}
	got2, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("失败后查询批次失败: %v", err)
	}
	if got2.RecipeNo != "R1" || got2.RecipeVersion != "v1" || got2.PlannedPortions != 10 {
		t.Fatalf("失败后应完整保留调整前批次，得到 %+v", got2)
	}
	mats2 := materialsMap(got2)
	if len(mats2) != 1 {
		t.Fatalf("应只保留原配方 M1，得到 %d 项: %+v", len(mats2), mats2)
	}
	checkRequirement(t, mats2, "M1", "1000", "0", "-1000")
}

// 失败的调整不占用请求编号：用同一编号修正为有效的版本与份数后应成功，
// 查询只显示这次成功调整的完整结果。
func TestUpdateDraftFailureDoesNotConsumeRequestNo(t *testing.T) {
	s := openTestStore(t)
	registerTwoRecipeVersions(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 同一编号先以不存在的版本提交（失败），再以有效版本与份数提交（成功）。
	if _, err := s.UpdateDraftBatch("u-fix", "B1", "R1", "v9", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的版本应返回 ErrNotFound，得到 %v", err)
	}
	view, err := s.UpdateDraftBatch("u-fix", "B1", "R1", "v2", 4)
	if err != nil {
		t.Fatalf("失败不应占用请求编号，修正后应成功: %v", err)
	}
	if view.RecipeVersion != "v2" || view.PlannedPortions != 4 {
		t.Fatalf("成功结果应指向有效调整: %+v", view)
	}

	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeVersion != "v2" || got.PlannedPortions != 4 || got.Status != StatusDraft {
		t.Fatalf("查询应只反映成功的调整: %+v", got)
	}
	mats := materialsMap(got)
	if len(mats) != 2 {
		t.Fatalf("应只含新版本 2 种物料，得到 %d 项", len(mats))
	}
	checkRequirement(t, mats, "M1", "1000", "0", "-1000") // 250 × 4
	checkRequirement(t, mats, "M4", "8", "0", "-8")       // 2 × 4

	// 溢出失败同样不占用编号：换用新编号先失败，再用同一编号改成可计算的一份。
	if _, err := s.RegisterRecipe("recipe-ov", "R9", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: "9223372036854775.807"},
	}); err != nil {
		t.Fatalf("登记上限配方失败: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-fix2", "B1", "R9", "v1", 2); err == nil {
		t.Fatal("两份溢出应失败，却成功了")
	}
	view2, err := s.UpdateDraftBatch("u-fix2", "B1", "R9", "v1", 1)
	if err != nil {
		t.Fatalf("溢出失败不应占用请求编号，修正为一份后应成功: %v", err)
	}
	if view2.PlannedPortions != 1 || view2.RecipeVersion != "v1" || view2.RecipeNo != "R9" {
		t.Fatalf("修正后的成功结果不正确: %+v", view2)
	}
}

// 保留局部调整的合法语义：配方编号与版本号都留空表示不改配方，
// 份数传 0 表示不改份数；这些调用不能被当成缺项拒绝。
// 同时确认开始执行后不能借组合调用重新调整计划。
func TestUpdateDraftPartialAdjustmentsStillValid(t *testing.T) {
	s := openTestStore(t)
	registerTwoRecipeVersions(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 只改份数，配方不动。
	v1, err := s.UpdateDraftBatch("u-p", "B1", "", "", 20)
	if err != nil {
		t.Fatalf("配方留空应表示不改配方: %v", err)
	}
	if v1.RecipeNo != "R1" || v1.RecipeVersion != "v1" || v1.PlannedPortions != 20 {
		t.Fatalf("只改份数的结果不正确: %+v", v1)
	}
	// 只改配方，份数传 0 不动份数。
	v2, err := s.UpdateDraftBatch("u-r", "B1", "R1", "v2", 0)
	if err != nil {
		t.Fatalf("份数传 0 应表示不改份数: %v", err)
	}
	if v2.RecipeNo != "R1" || v2.RecipeVersion != "v2" || v2.PlannedPortions != 20 {
		t.Fatalf("只改配方的结果不正确: %+v", v2)
	}
	// 两者都缺省：合法的无变化调用，原样返回。
	v3, err := s.UpdateDraftBatch("u-nop", "B1", "", "", 0)
	if err != nil {
		t.Fatalf("配方与份数都缺省应为合法的无变化调整: %v", err)
	}
	if v3.RecipeVersion != "v2" || v3.PlannedPortions != 20 || v3.Status != StatusDraft {
		t.Fatalf("无变化调整结果不正确: %+v", v3)
	}

	// 开始执行后，组合调整同样不被允许。
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-after", "B1", "R1", "v1", 30); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("执行后组合调整应返回 ErrInvalidState，得到 %v", err)
	}
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeVersion != "v2" || got.PlannedPortions != 20 || got.Status != StatusExecuting {
		t.Fatalf("执行后的非法调整不应改动批次: %+v", got)
	}
}
