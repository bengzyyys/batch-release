package release

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// 本文件是“已登记配方版本内容不可覆盖”的回归保障，重点保护调用方拿到的
// 物料列表/配方视图与台账之间的隔离。批次查询结果的隔离已由 TestViewIsolation
// 覆盖，这里补齐三处入口：登记输入、登记结果、配方查询结果，并进一步验证
// 引用该版本的批次（既有批次与之后新建的批次）始终以已登记内容作为投料依据。
//
// 约定的已登记内容（R1/v1「配方一」）：物料顺序 M1=100 克、M2=0.5 克。
// 调用方可以任意整理手里的本地对象，但任何整理都不得：
//   - 改写已经登记的版本（名称、物料顺序、每份用量、物料项数）；
//   - 让先取得的另一份查询结果出现被改过的物料、漏项或额外物料；
//   - 改变引用该版本的批次据以核对的应投量、实投归属与差额；
//   - 借“再次登记”覆盖已存在的编号+版本（仍是 ErrRecipeExists）。

// isoBaseMaterials 返回登记 R1/v1 使用的原始物料输入（每份新建一个切片，
// 方便各用例独立地对自己手里的列表做破坏性整理）。
func isoBaseMaterials() []MaterialInput {
	return []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}
}

// isoWantMaterials 是登记成功那一刻的物料视图：顺序与每份用量都固定。
func isoWantMaterials() []MaterialView {
	return []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}
}

// assertRecipeViewExactly 断言配方视图与给定内容逐项一致：编号、版本、名称，
// 以及物料的编号、每份克数和顺序；多一项、少一项、换序或改值都判失败。
func assertRecipeViewExactly(t *testing.T, r *RecipeView, recipeNo, version, name string, want []MaterialView) {
	t.Helper()
	if r == nil {
		t.Fatal("配方视图为 nil")
	}
	if r.RecipeNo != recipeNo || r.Version != version {
		t.Fatalf("配方编号/版本应为 %q/%q，得到 %q/%q", recipeNo, version, r.RecipeNo, r.Version)
	}
	if r.Name != name {
		t.Fatalf("配方 %q/%q 的名称应为 %q，得到 %q", recipeNo, version, name, r.Name)
	}
	if len(r.Materials) != len(want) {
		t.Fatalf("配方 %q/%q 应有 %d 项物料（无漏项/额外项），得到 %d 项: %+v",
			recipeNo, version, len(want), len(r.Materials), r.Materials)
	}
	for i, w := range want {
		if r.Materials[i] != w {
			t.Fatalf("配方 %q/%q 第 %d 项物料应为 %+v，得到 %+v；完整列表: %+v",
				recipeNo, version, i+1, w, r.Materials[i], r.Materials)
		}
	}
}

// assertRegisteredRecipeV1 断言 R1/v1 仍是登记成功那一刻的内容。
func assertRegisteredRecipeV1(t *testing.T, r *RecipeView) {
	t.Helper()
	assertRecipeViewExactly(t, r, "R1", "v1", "配方一", isoWantMaterials())
}

// 登记输入隔离：登记成功后，调用方随意整理原来的物料列表——改物料编号、
// 改每份克数、调整顺序、替换其中一项、追加新项——台账里的版本必须保持
// 登记成功时的名称、物料顺序和每份用量；即使关闭后重新打开也不改变。
func TestRecipeRegisterInputCopyIsolation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) 失败: %v", dir, err)
	}

	materials := isoBaseMaterials()
	view, err := s.RegisterRecipe("req-register", "R1", "v1", "配方一", materials)
	if err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
	assertRegisteredRecipeV1(t, view)

	// 登记调用早已返回，此后对原输入切片做各种“本地整理”。
	materials[0].MaterialNo = "M9" // 改物料编号
	materials[0].Grams = "777"    // 连同该项的每份克数一起改
	materials[1].Grams = "999"    // 改另一项的每份克数
	materials[0], materials[1] = materials[1], materials[0] // 调整顺序
	materials[1] = MaterialInput{MaterialNo: "M7", Grams: "7"} // 替换其中一项
	materials = append(materials, MaterialInput{MaterialNo: "M8", Grams: "8"}) // 追加一项

	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("整理本地输入后查询配方失败: %v", err)
	}
	assertRegisteredRecipeV1(t, got)

	// 同一请求编号按“原内容”重放，返回的仍是第一次成功时的结果——
	// 落盘的请求结果同样不被调用方手里的切片牵动。
	replay, err := s.RegisterRecipe("req-register", "R1", "v1", "配方一", isoBaseMaterials())
	if err != nil {
		t.Fatalf("按原内容幂等重放应成功: %v", err)
	}
	assertRegisteredRecipeV1(t, replay)

	// 关闭重开：本地切片的变化从未进入落盘内容。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	defer s2.Close()
	reopened, err := s2.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("重新打开后查询配方失败: %v", err)
	}
	assertRegisteredRecipeV1(t, reopened)
}

// 登记结果隔离：调用方改登记结果的名称、改物料内容、追加或清空物料列表，
// 都只能影响手里的这个对象；后续查询得到的仍是完整的登记内容。
func TestRecipeRegisterResultCopyIsolation(t *testing.T) {
	s := openTestStore(t)
	view, err := s.RegisterRecipe("req-register", "R1", "v1", "配方一", isoBaseMaterials())
	if err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}

	// 对“登记成功拿到的对象”做破坏性整理。
	view.Name = "本地改名"
	view.Materials[0].MaterialNo = "M9"
	view.Materials[0].Grams = "777"
	view.Materials[1] = MaterialView{MaterialNo: "M8", Grams: "8"} // 替换一项
	view.Materials = append(view.Materials, MaterialView{MaterialNo: "M6", Grams: "6"})
	view.Materials = nil // 清空物料列表

	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("整理登记结果后查询配方失败: %v", err)
	}
	assertRegisteredRecipeV1(t, got)

	// 幂等重放返回的结果也不受此前清空影响：台账另存了一份结果。
	replay, err := s.RegisterRecipe("req-register", "R1", "v1", "配方一", isoBaseMaterials())
	if err != nil {
		t.Fatalf("幂等重放应成功: %v", err)
	}
	assertRegisteredRecipeV1(t, replay)
}

// 配方查询结果隔离：先取得两份查询结果，对其中一份改名称、改物料内容、
// 追加、清空，另一份保持原内容；两份都改坏之后，后续查询仍得到完整配方，
// 不出现被改过的物料、漏项或额外物料。
func TestRecipeQueryResultCopyIsolation(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("req-register", "R1", "v1", "配方一", isoBaseMaterials()); err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}

	first, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}

	// 整理先取得的这一份。
	first.Name = "本地改名"
	first.Materials[0].MaterialNo = "M9"
	first.Materials[0].Grams = "777"
	first.Materials[1].Grams = "888"
	first.Materials = append(first.Materials, MaterialView{MaterialNo: "M5", Grams: "5"})

	// 先取得的另一份查询结果应保持原内容，不与第一份共享底层数组。
	assertRegisteredRecipeV1(t, second)

	// 清空第一份，台账与第二份仍不受影响。
	first.Materials = nil
	assertRegisteredRecipeV1(t, second)

	// 再把第二份也清空并追加一个本地物料、改本地名称。
	second.Materials = second.Materials[:0]
	second.Materials = append(second.Materials, MaterialView{MaterialNo: "MX", Grams: "1"})
	second.Name = "另一个本地名"
	if len(second.Materials) != 1 || second.Materials[0].MaterialNo != "MX" {
		t.Fatalf("本地整理应当真的只作用于手中对象，得到 %+v", second.Materials)
	}

	// 后续查询仍得到完整配方：M1=100、M2=0.5，顺序不变，无 MX/M5/M9。
	third, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	assertRegisteredRecipeV1(t, third)
}

// 配方是批次投料依据：原版本为每份 M1=100 克、M2=0.5 克，批次计划 3 份。
// 调用方把手中配方里的 M1 改成其他编号/克数并追加物料后：
//   - 既有批次仍列出 M1 应投 300 克、M2 应投 1.5 克，未投料的 M2 仍显示；
//   - 已登记的实际投料与差额仍按原版本核对，本地变化不会重新解释物料；
//   - 改过的编号 M9 不能用于投料（ErrMaterialNotInRecipe）；
//   - 此后新建、绑定同一版本的批次同样采用已登记内容；
//   - 关闭重开后，落盘内容仍是登记时的版本与投料。
func TestBatchRequirementsUseRegisteredRecipeVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) 失败: %v", dir, err)
	}
	held, err := s.RegisterRecipe("req-register", "R1", "v1", "配方一", isoBaseMaterials())
	if err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatalf("创建既有批次失败: %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	fixedTime := time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC)
	// M1 应投 300 克，先投 250 克（欠 50）；M2 不投料。
	if _, err := s.AddFeeding("f1", "B1", "M1", "250", fixedTime, "张三"); err != nil {
		t.Fatalf("登记 M1 投料失败: %v", err)
	}

	// 调用方把“手中配方”改得面目全非：M1 换编号、换克数，M2 换克数，
	// 追加一项，改名称。
	held.Materials[0].MaterialNo = "M9"
	held.Materials[0].Grams = "999"
	held.Materials[1].Grams = "888"
	held.Materials = append(held.Materials, MaterialView{MaterialNo: "M5", Grams: "5"})
	held.Name = "本地改名"

	// 本地改成 M9 不会让台账接受 M9 投料：归属仍按登记版本解释。
	if _, err := s.AddFeeding("f-bad", "B1", "M9", "10", fixedTime, "张三"); !errors.Is(err, ErrMaterialNotInRecipe) {
		t.Fatalf("本地改名后的 M9 不应属于登记版本，应返回 ErrMaterialNotInRecipe，得到 %v", err)
	}

	assertExistingBatch := func(t *testing.T, st *Store) {
		t.Helper()
		got, err := st.GetBatch("B1")
		if err != nil {
			t.Fatalf("查询批次失败: %v", err)
		}
		if got.RecipeNo != "R1" || got.RecipeVersion != "v1" || got.RecipeName != "配方一" {
			t.Fatalf("批次绑定的配方信息不应改变，得到 %q %q %q", got.RecipeNo, got.RecipeVersion, got.RecipeName)
		}
		if len(got.Materials) != 2 {
			t.Fatalf("应只列出登记版本的 2 项物料（无 M9/M5，M2 不漏），得到 %+v", got.Materials)
		}
		m := materialsMap(got)
		// 应投 100×3=300，实投仍是登记的 250，差额 -50。
		checkRequirement(t, m, "M1", "300", "250", "-50")
		// 应投 0.5×3=1.5，未投料仍显示，实投 0，差额 -1.5。
		checkRequirement(t, m, "M2", "1.5", "0", "-1.5")
		if len(got.Feedings) != 1 {
			t.Fatalf("被拒绝的 M9 投料不应落账，得到 %d 条投料", len(got.Feedings))
		}
		f := got.Feedings[0]
		if f.Seq != 1 || f.MaterialNo != "M1" || f.Grams != "250" {
			t.Fatalf("已登记投料不应被本地变化重新解释，得到 %+v", f)
		}
	}
	assertExistingBatch(t, s)

	// 此后新建、绑定同一已登记版本的批次，仍按 M1=100、M2=0.5 计应投。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 3); err != nil {
		t.Fatalf("本地整理后新建批次应仍能绑定登记版本: %v", err)
	}
	got2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	if got2.RecipeName != "配方一" || len(got2.Materials) != 2 {
		t.Fatalf("新批次应采用登记版本，得到 %+v", got2)
	}
	m2 := materialsMap(got2)
	checkRequirement(t, m2, "M1", "300", "0", "-300")
	checkRequirement(t, m2, "M2", "1.5", "0", "-1.5")

	// 关闭重开：丢弃全部本地对象后，台账内容仍是登记时的版本与投料。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	defer s3.Close()
	reopenedRecipe, err := s3.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	assertRegisteredRecipeV1(t, reopenedRecipe)
	assertExistingBatch(t, s3)
}

// 版本间隔离：同一配方编号可以有多个版本，两个版本即使使用相同物料编号，
// 每份克数也各自独立。改动一个版本的返回对象，既不能影响另一个版本的查询
// 结果，也不能让绑定各版本的批次改用另一版本的用量。
func TestRecipeVersionViewsIndependent(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("rv1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatalf("登记 v1 失败: %v", err)
	}
	// v2 也使用 M1，但每份 250 克；另有 M4=2 克。
	if _, err := s.RegisterRecipe("rv2", "R1", "v2", "配方改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M4", Grams: "2"},
	}); err != nil {
		t.Fatalf("登记 v2 失败: %v", err)
	}

	v1, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.GetRecipe("R1", "v2")
	if err != nil {
		t.Fatal(err)
	}

	// 对 v1 的返回对象做破坏性整理：M1 改编号、改克数，M2 改克数，追加，清空。
	v1.Name = "本地改名"
	v1.Materials[0].MaterialNo = "M9"
	v1.Materials[0].Grams = "999"
	v1.Materials[1].Grams = "888"
	v1.Materials = append(v1.Materials, MaterialView{MaterialNo: "M5", Grams: "5"})
	v1.Materials = nil

	// v2 手里的对象不受影响：仍是 M1=250、M4=2。
	assertRecipeViewExactly(t, v2, "R1", "v2", "配方改版", []MaterialView{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M4", Grams: "2"},
	})
	// v1 重新查询仍是登记内容。
	again1, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	assertRecipeViewExactly(t, again1, "R1", "v1", "配方初版", []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})

	// 两个批次分别绑定两个版本：相同编号 M1 的应投量各按各的版本计算。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 2); err != nil {
		t.Fatal(err)
	}

	// 再整理手里的 v2：把 M1 改成 1 克并追加物料。
	v2.Materials[0].Grams = "1"
	v2.Materials = append(v2.Materials, MaterialView{MaterialNo: "M6", Grams: "6"})
	v2.Name = "本地改版名"

	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	if b1.RecipeName != "配方初版" || b2.RecipeName != "配方改版" {
		t.Fatalf("批次不应采用被本地改名的版本: %q / %q", b1.RecipeName, b2.RecipeName)
	}
	mb1 := materialsMap(b1)
	if len(mb1) != 2 {
		t.Fatalf("v1 批次应只含 M1、M2，得到 %+v", b1.Materials)
	}
	checkRequirement(t, mb1, "M1", "300", "0", "-300") // 100×3，不借 v2 的 250
	checkRequirement(t, mb1, "M2", "1.5", "0", "-1.5")
	mb2 := materialsMap(b2)
	if len(mb2) != 2 {
		t.Fatalf("v2 批次应只含 M1、M4，得到 %+v", b2.Materials)
	}
	checkRequirement(t, mb2, "M1", "500", "0", "-500") // 250×2，不借 v1 的 100，也不是本地改的 1
	checkRequirement(t, mb2, "M4", "4", "0", "-4")     // 2×2

	// 两个版本再查一次，均为登记内容，互不串改。
	last1, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	last2, err := s.GetRecipe("R1", "v2")
	if err != nil {
		t.Fatal(err)
	}
	assertRecipeViewExactly(t, last1, "R1", "v1", "配方初版", []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
	assertRecipeViewExactly(t, last2, "R1", "v2", "配方改版", []MaterialView{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M4", Grams: "2"},
	})
}

// 本地改动不等于正式提交：
//   - 用“同一请求编号 + 改过的内容”再提交，是 ErrRequestConflict（不同内容），
//     不会借幂等通道覆盖；
//   - 用“新的请求编号”把改过的内容再次登记到已存在的配方编号+版本号，
//     无论改名称、改克数、改编号、换序、替换、追加、删减，乃至内容本身非法，
//     都先撞上“版本已存在”，统一返回 ErrRecipeExists。
// 原版本与引用它的批次始终保持原样。
func TestResubmitModifiedContentStillExists(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("req-first", "R1", "v1", "配方一", isoBaseMaterials()); err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}

	// 同一请求编号配改过的内容：按请求冲突拒绝，而不是覆盖或重放。
	tampered := []MaterialInput{
		{MaterialNo: "M1", Grams: "200"},
		{MaterialNo: "M2", Grams: "0.5"},
	}
	if _, err := s.RegisterRecipe("req-first", "R1", "v1", "配方一", tampered); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同内容应返回 ErrRequestConflict，得到 %v", err)
	}

	// 新请求编号 + 各种改过（甚至非法）的内容再次登记到 R1/v1：一律 ErrRecipeExists。
	cases := []struct {
		name       string
		recipeName string
		materials  []MaterialInput
	}{
		{"改每份克数", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "200"},
			{MaterialNo: "M2", Grams: "0.5"},
		}},
		{"改物料编号", "配方一", []MaterialInput{
			{MaterialNo: "MX", Grams: "100"},
			{MaterialNo: "M2", Grams: "0.5"},
		}},
		{"改配方名称", "配方改名", isoBaseMaterials()},
		{"调整物料顺序", "配方一", []MaterialInput{
			{MaterialNo: "M2", Grams: "0.5"},
			{MaterialNo: "M1", Grams: "100"},
		}},
		{"替换其中一项", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "100"},
			{MaterialNo: "M7", Grams: "7"},
		}},
		{"追加物料", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "100"},
			{MaterialNo: "M2", Grams: "0.5"},
			{MaterialNo: "M3", Grams: "3"},
		}},
		{"删减物料", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "100"},
		}},
		{"内容非法_物料编号重复", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "1"},
			{MaterialNo: "M1", Grams: "2"},
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqNo := fmt.Sprintf("req-again-%d", i)
			if _, err := s.RegisterRecipe(reqNo, "R1", "v1", tc.recipeName, tc.materials); !errors.Is(err, ErrRecipeExists) {
				t.Fatalf("改过的内容再次登记到已存在版本应返回 ErrRecipeExists，得到 %v", err)
			}
		})
	}

	// 原版本内容不变。
	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	assertRegisteredRecipeV1(t, got)

	// 引用它的批次仍按登记版本核对：M1=300、M2=1.5，均无投料。
	batch, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if batch.RecipeName != "配方一" || len(batch.Materials) != 2 {
		t.Fatalf("批次应保持绑定登记版本，得到 %+v", batch)
	}
	m := materialsMap(batch)
	checkRequirement(t, m, "M1", "300", "0", "-300")
	checkRequirement(t, m, "M2", "1.5", "0", "-1.5")
}
