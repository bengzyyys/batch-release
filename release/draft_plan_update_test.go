package release

import (
	"errors"
	"testing"
)

// materialsByNo 把数量核对结果按物料编号索引，便于逐项断言。
func materialsByNo(view *BatchView) map[string]MaterialRequirement {
	m := make(map[string]MaterialRequirement, len(view.Materials))
	for _, mm := range view.Materials {
		m[mm.MaterialNo] = mm
	}
	return m
}

func checkRequirement(t *testing.T, view *BatchView, mat, req, act, diff string) {
	t.Helper()
	got := materialsByNo(view)
	m, ok := got[mat]
	if !ok {
		t.Fatalf("缺少物料 %q，当前核对项: %+v", mat, view.Materials)
	}
	if m.RequiredGrams != req || m.ActualGrams != act || m.DifferenceGrams != diff {
		t.Fatalf("物料 %q 核对应为 应投=%s 实投=%s 差额=%s，得到 %+v", mat, req, act, diff, m)
	}
}

// 一次提交同时改选配方版本和计划份数：两项一起生效，
// 数量核对完全按新版本的物料与每份克数 × 新份数计算。
func TestUpdateDraftRecipeAndPortionsTogether(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1") // R1 v1：M1=100, M2=0.5, M3=0.010
	// R2 v1：M2 每份克数不同（2），去掉 M1/M3，新增 M4。
	if _, err := s.RegisterRecipe("recipe-2", "R2", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M2", Grams: "2"},
		{MaterialNo: "M4", Grams: "0.25"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	view, err := s.UpdateDraftBatch("u1", "B1", "R2", "v1", 4)
	if err != nil {
		t.Fatalf("同时调整配方版本和份数失败: %v", err)
	}
	if view.BatchNo != "B1" || view.RecipeNo != "R2" || view.RecipeVersion != "v1" ||
		view.RecipeName != "新配方" || view.PlannedPortions != 4 || view.Status != StatusDraft {
		t.Fatalf("调整后的批次视图不正确: %+v", view)
	}
	if len(view.Feedings) != 0 {
		t.Fatalf("草稿不应有投料，得到 %d 条", len(view.Feedings))
	}
	// 新版本没有的旧物料（M1、M3）不应残留；新增物料 M4 未投料也要列出。
	if len(view.Materials) != 2 {
		t.Fatalf("应只列出新版本的 2 种物料，得到 %+v", view.Materials)
	}
	// M2 每份克数按新值 2 计算：2 × 4 = 8，不能只改版本文字。
	checkRequirement(t, view, "M2", "8", "0", "-8")
	// M4：0.25 × 4 = 1；草稿实投为零，差额为应投量的负值。
	checkRequirement(t, view, "M4", "1", "0", "-1")

	// 随后查询应指向同一个新配方版本、新计划份数，结果与返回视图一致。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeNo != "R2" || got.RecipeVersion != "v1" || got.PlannedPortions != 4 ||
		got.Status != StatusDraft || len(got.Materials) != 2 {
		t.Fatalf("查询结果应与调整返回一致: %+v", got)
	}
	checkRequirement(t, got, "M2", "8", "0", "-8")
	checkRequirement(t, got, "M4", "1", "0", "-1")

	// 原来登记的两个配方版本本身不能被此次调整改写。
	r1, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Name != "标准配方" || len(r1.Materials) != 3 ||
		r1.Materials[0].Grams != "100" || r1.Materials[1].Grams != "0.5" || r1.Materials[2].Grams != "0.01" {
		t.Fatalf("原配方版本不应被改写: %+v", r1)
	}
	r2, err := s.GetRecipe("R2", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Name != "新配方" || len(r2.Materials) != 2 ||
		r2.Materials[0].Grams != "2" || r2.Materials[1].Grams != "0.25" {
		t.Fatalf("目标配方版本不应被改写: %+v", r2)
	}
}

// 新份数有效但选中了不存在的配方版本：整体返回 ErrNotFound，
// 批次保留调整前的完整内容；失败不占用请求编号，修正后可成功。
func TestUpdateDraftUnknownRecipeKeepsBatch(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1") // R1 v1：M1=100, M2=0.5, M3=0.010
	if _, err := s.RegisterRecipe("recipe-2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M1", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 份数 20 本身合法，但配方版本不存在 → 整体失败。
	if _, err := s.UpdateDraftBatch("u1", "B1", "R9", "v9", 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的配方版本应返回 ErrNotFound，得到 %v", err)
	}

	// 查询仍显示原份数、原配方和原有数量核对结果。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeNo != "R1" || got.RecipeVersion != "v1" || got.PlannedPortions != 10 ||
		got.Status != StatusDraft || len(got.Materials) != 3 {
		t.Fatalf("失败的调整不应留下任何变化: %+v", got)
	}
	checkRequirement(t, got, "M1", "1000", "0", "-1000")
	checkRequirement(t, got, "M2", "5", "0", "-5")
	checkRequirement(t, got, "M3", "0.1", "0", "-0.1")

	// 同一请求编号修正为有效的配方版本和份数后应能成功。
	view, err := s.UpdateDraftBatch("u1", "B1", "R2", "v1", 20)
	if err != nil {
		t.Fatalf("失败不应占用请求编号，修正后应成功: %v", err)
	}
	if view.RecipeNo != "R2" || view.RecipeVersion != "v1" || view.PlannedPortions != 20 {
		t.Fatalf("修正后的视图不正确: %+v", view)
	}
	// 查询只显示这次成功调整的完整结果。
	got, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeNo != "R2" || got.RecipeVersion != "v1" || got.PlannedPortions != 20 ||
		len(got.Materials) != 1 {
		t.Fatalf("查询应只显示成功调整后的结果: %+v", got)
	}
	checkRequirement(t, got, "M1", "40", "0", "-40")
}

// 目标版本存在，但按新份数计算后某物料应投量超出可表示范围：整体拒绝，
// 不留下已改好的份数或配方，不出现回绕数量或部分物料；上限按物料分别判断。
func TestUpdateDraftOverflowKeepsBatch(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("recipe-1", "R1", "v1", "轻配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	// 每份 M1 需要 9223372036854775.807 克（上限）：一份可以计算，两份溢出。
	if _, err := s.RegisterRecipe("recipe-2", "R2", "v1", "重配方", []MaterialInput{
		{MaterialNo: "M1", Grams: limitGrams},
		{MaterialNo: "M2", Grams: "3"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 同时改到重配方并改为 2 份：M1 应投量溢出，整体必须被拒绝。
	if _, err := s.UpdateDraftBatch("u1", "B1", "R2", "v1", 2); err == nil {
		t.Fatal("应投量超出可表示范围时应返回错误")
	}

	// 不能留下已经改好的份数或配方，也不能出现回绕后的数量或部分物料。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("失败的调整不应破坏查询: %v", err)
	}
	if got.RecipeNo != "R1" || got.RecipeVersion != "v1" || got.PlannedPortions != 10 ||
		got.Status != StatusDraft || len(got.Materials) != 1 {
		t.Fatalf("溢出失败后批次应保持调整前的完整内容: %+v", got)
	}
	checkRequirement(t, got, "M1", "10", "0", "-10")

	// 同一编号修正为 1 份后应能成功：一份的应投量恰好等于上限，可以计算。
	view, err := s.UpdateDraftBatch("u1", "B1", "R2", "v1", 1)
	if err != nil {
		t.Fatalf("一份恰好等于上限应成功: %v", err)
	}
	checkRequirement(t, view, "M1", limitGrams, "0", "-"+limitGrams)
	checkRequirement(t, view, "M2", "3", "0", "-3")

	// 已绑定重配方后再改为 2 份，同样整体拒绝，原有一份的结果不变。
	if _, err := s.UpdateDraftBatch("u2", "B1", "", "", 2); err == nil {
		t.Fatal("改为两份应返回错误")
	}
	got, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeNo != "R2" || got.RecipeVersion != "v1" || got.PlannedPortions != 1 ||
		len(got.Materials) != 2 {
		t.Fatalf("再次溢出失败后批次应保持一份时的结果: %+v", got)
	}
	checkRequirement(t, got, "M1", limitGrams, "0", "-"+limitGrams)
	checkRequirement(t, got, "M2", "3", "0", "-3")

	// 上限按物料分别判断：两种物料各自都达到上限仍可调整成功，
	// 不能把它们的应投量相加后判断。
	if _, err := s.RegisterRecipe("recipe-3", "R3", "v1", "双重配方", []MaterialInput{
		{MaterialNo: "M1", Grams: limitGrams},
		{MaterialNo: "M2", Grams: limitGrams},
	}); err != nil {
		t.Fatal(err)
	}
	view, err = s.UpdateDraftBatch("u3", "B1", "R3", "v1", 1)
	if err != nil {
		t.Fatalf("各物料分别不超上限时应成功: %v", err)
	}
	checkRequirement(t, view, "M1", limitGrams, "0", "-"+limitGrams)
	checkRequirement(t, view, "M2", limitGrams, "0", "-"+limitGrams)
}

// 局部调整的语义不变：配方编号和版本号都留空表示不改配方，份数传 0 表示不改份数；
// 开始执行后的批次不能通过组合调整重新获得改计划的能力。
func TestUpdateDraftNoChangeAndExecutingGuard(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.RegisterRecipe("recipe-2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M1", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 全部留空/传零是合法调用：不改配方也不改份数，不能当成缺项拒绝。
	view, err := s.UpdateDraftBatch("u1", "B1", "", "", 0)
	if err != nil {
		t.Fatalf("全部留空应表示不调整，得到 %v", err)
	}
	if view.RecipeNo != "R1" || view.RecipeVersion != "v1" || view.PlannedPortions != 10 {
		t.Fatalf("不调整的调用不应改变批次: %+v", view)
	}

	// 组合调整成功。
	if _, err := s.UpdateDraftBatch("u2", "B1", "R2", "v1", 5); err != nil {
		t.Fatal(err)
	}
	// 组合调整后再传全空仍合法，保持组合调整的结果。
	view, err = s.UpdateDraftBatch("u3", "B1", "", "", 0)
	if err != nil {
		t.Fatalf("组合调整后全部留空仍应合法: %v", err)
	}
	if view.RecipeNo != "R2" || view.RecipeVersion != "v1" || view.PlannedPortions != 5 {
		t.Fatalf("不调整的调用不应改变组合调整的结果: %+v", view)
	}

	// 开始执行后，组合调整同样被拒绝，批次保持执行前的计划。
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u4", "B1", "R1", "v1", 99); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("执行中组合调整应返回 ErrInvalidState，得到 %v", err)
	}
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeNo != "R2" || got.RecipeVersion != "v1" || got.PlannedPortions != 5 ||
		got.Status != StatusExecuting {
		t.Fatalf("执行中的批次不应被重新调整计划: %+v", got)
	}
}
