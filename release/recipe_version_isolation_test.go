package release

import (
	"errors"
	"testing"
	"time"
)

// 本文件是“配方版本内容不可覆盖”的回归保障：调用方手中的物料列表
// （登记输入、登记结果、查询结果）都是本地对象，整理它们不能改写已登记
// 的版本，也不能改变引用该版本的批次所依据的用量。批次查询结果的隔离
// 由 TestViewIsolation 覆盖，这里补齐配方一侧的对应保障。

// checkRecipeView 校验配方查询结果与登记成功时完全一致：
// 名称、物料顺序、每种物料的编号与每份克数。
func checkRecipeView(t *testing.T, got *RecipeView, wantName string, want []MaterialView) {
	t.Helper()
	if got.Name != wantName {
		t.Fatalf("配方名称应为 %q，得到 %q", wantName, got.Name)
	}
	if len(got.Materials) != len(want) {
		t.Fatalf("物料应为 %d 项，得到 %d 项: %+v", len(want), len(got.Materials), got.Materials)
	}
	for i, m := range want {
		if got.Materials[i] != m {
			t.Fatalf("第 %d 项物料应为 %+v，得到 %+v", i, m, got.Materials[i])
		}
	}
}

// checkBatchRequirements 校验批次查询结果中每种物料的应投/实投/差额，
// 顺序与配方物料一致，无投料的物料也必须列出。
func checkBatchRequirements(t *testing.T, got *BatchView, want []MaterialRequirement) {
	t.Helper()
	if len(got.Materials) != len(want) {
		t.Fatalf("批次应列出 %d 项物料核对，得到 %d 项: %+v", len(want), len(got.Materials), got.Materials)
	}
	for i, m := range want {
		if got.Materials[i] != m {
			t.Fatalf("第 %d 项核对应为 %+v，得到 %+v", i, m, got.Materials[i])
		}
	}
}

// 登记输入隔离：登记成功后修改调用方原来的物料列表（改编号、改克数、
// 调顺序、替换整项），重新查询仍应看到登记成功时的名称、顺序与用量。
func TestRecipeInputIsolation(t *testing.T) {
	s := openTestStore(t)

	materials := []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "原始配方", materials); err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}

	// 调用方整理手中的输入列表：改编号、改克数、调顺序、替换整项、追加。
	materials[0].MaterialNo = "M9"
	materials[0].Grams = "999"
	materials[1] = MaterialInput{MaterialNo: "M8", Grams: "7"}
	materials = append(materials, MaterialInput{MaterialNo: "M7", Grams: "1"})
	_ = materials

	want := []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}
	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("查询配方失败: %v", err)
	}
	checkRecipeView(t, got, "原始配方", want)
}

// 登记结果与查询结果隔离：对返回的配方视图改名称、改物料内容、清空或
// 追加物料列表，只影响手中的对象；先取得的另一份查询结果保持原内容，
// 后续查询仍得到完整配方。
func TestRecipeViewIsolation(t *testing.T) {
	s := openTestStore(t)

	regView, err := s.RegisterRecipe("r1", "R1", "v1", "原始配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})
	if err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
	// 先取得一份查询结果，之后对其他对象的改动不能波及它。
	before, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	queryView, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}

	// 改登记结果：名称、物料编号、每份克数，并追加一项。
	regView.Name = "被改名"
	regView.Materials[0].MaterialNo = "M9"
	regView.Materials[0].Grams = "999"
	regView.Materials = append(regView.Materials, MaterialView{MaterialNo: "FAKE", Grams: "1"})
	// 改查询结果：物料内容，并清空物料列表。
	queryView.Name = "也被改名"
	queryView.Materials[1].MaterialNo = "M8"
	queryView.Materials[1].Grams = "7"
	queryView.Materials = nil

	want := []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}
	// 先取得的查询结果保持原内容。
	checkRecipeView(t, before, "原始配方", want)
	// 后续查询仍得到完整配方：没有被改过的物料、漏项或额外物料。
	again, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	checkRecipeView(t, again, "原始配方", want)
}

// 批次投料依据隔离：调用方改动手中的配方视图后，既有批次与新建批次
// 仍按已登记版本核对用量；已登记的投料及其差额按原版本核对，未投料
// 的物料仍然显示。
func TestRecipeMutationDoesNotAffectBatchBasis(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "投料配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// 已登记的实际投料：M1 投 150（应投 300，差 -150），M2 不投料。
	if _, err := s.AddFeeding("f1", "B1", "M1", "150", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}

	// 调用方把手中配方里的 M1 改成其他编号、其他克数。
	view, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	view.Materials[0].MaterialNo = "M9"
	view.Materials[0].Grams = "7"

	want := []MaterialRequirement{
		{MaterialNo: "M1", RequiredGrams: "300", ActualGrams: "150", DifferenceGrams: "-150"},
		{MaterialNo: "M2", RequiredGrams: "1.5", ActualGrams: "0", DifferenceGrams: "-1.5"},
	}
	// 既有批次仍按原版本核对：M1 应投 300、M2 应投 1.5，投料与差额不变，
	// 未投料的 M2 仍然显示。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	checkBatchRequirements(t, got, want)
	if len(got.Feedings) != 1 || got.Feedings[0].MaterialNo != "M1" || got.Feedings[0].Grams != "150" {
		t.Fatalf("已登记投料不应被重新解释: %+v", got.Feedings)
	}

	// 本地改名不产生新物料：M9 仍不属于该批次的配方版本。
	if _, err := s.AddFeeding("f2", "B1", "M9", "1", time.Now(), "张三"); !errors.Is(err, ErrMaterialNotInRecipe) {
		t.Fatalf("本地改出的物料不应被接受，得到 %v", err)
	}
	// 原物料仍可正常投料。
	if _, err := s.AddFeeding("f3", "B1", "M1", "50", time.Now(), "李四"); err != nil {
		t.Fatalf("原物料投料失败: %v", err)
	}
	got, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	checkBatchRequirements(t, got, []MaterialRequirement{
		{MaterialNo: "M1", RequiredGrams: "300", ActualGrams: "200", DifferenceGrams: "-100"},
		{MaterialNo: "M2", RequiredGrams: "1.5", ActualGrams: "0", DifferenceGrams: "-1.5"},
	})

	// 此后新建、绑定同一版本的批次也采用已登记内容。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	checkBatchRequirements(t, got2, []MaterialRequirement{
		{MaterialNo: "M1", RequiredGrams: "300", ActualGrams: "0", DifferenceGrams: "-300"},
		{MaterialNo: "M2", RequiredGrams: "1.5", ActualGrams: "0", DifferenceGrams: "-1.5"},
	})
}

// 多版本隔离：同一配方编号的两个版本使用相同物料编号但用量不同；
// 改动某个版本的返回对象不影响另一个版本，批次也不改用另一版本的用量。
func TestRecipeVersionIsolationAcrossVersions(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "旧版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r2", "R1", "v2", "新版", []MaterialInput{
		{MaterialNo: "M1", Grams: "2"},
		{MaterialNo: "M2", Grams: "9"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}

	// 改动 v1 的返回对象：把 M1 的克数改成 v2 的用量，名称也换掉。
	v1, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	v1.Name = "新版"
	v1.Materials[0].Grams = "2"
	v1.Materials[1].Grams = "9"

	// 另一个版本不受波及。
	v2, err := s.GetRecipe("R1", "v2")
	if err != nil {
		t.Fatal(err)
	}
	checkRecipeView(t, v2, "新版", []MaterialView{
		{MaterialNo: "M1", Grams: "2"},
		{MaterialNo: "M2", Grams: "9"},
	})
	// 被改对象的正式内容仍是登记时的样子。
	v1Again, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	checkRecipeView(t, v1Again, "旧版", []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	})

	// 两个批次各自按绑定版本的用量核对，互不串用。
	got1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	checkBatchRequirements(t, got1, []MaterialRequirement{
		{MaterialNo: "M1", RequiredGrams: "300", ActualGrams: "0", DifferenceGrams: "-300"},
		{MaterialNo: "M2", RequiredGrams: "1.5", ActualGrams: "0", DifferenceGrams: "-1.5"},
	})
	got2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	checkBatchRequirements(t, got2, []MaterialRequirement{
		{MaterialNo: "M1", RequiredGrams: "6", ActualGrams: "0", DifferenceGrams: "-6"},
		{MaterialNo: "M2", RequiredGrams: "27", ActualGrams: "0", DifferenceGrams: "-27"},
	})
}

// 本地改动不等于正式提交：把改过的内容用新的请求编号再次登记到已存在
// 的配方编号和版本号，仍返回 ErrRecipeExists，原版本与引用它的批次
// 保持原样；改动也不会随后续写入被持久化。
func TestReregisterMutatedContentStillRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "原始配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}

	// 调用方在手中视图上改名、改物料、调顺序。
	view, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	view.Name = "篡改配方"
	view.Materials[0], view.Materials[1] = view.Materials[1], view.Materials[0]
	view.Materials[0].Grams = "1"

	// 用新的请求编号把改过的内容登记到已存在的编号和版本号。
	mutated := make([]MaterialInput, 0, len(view.Materials))
	for _, m := range view.Materials {
		mutated = append(mutated, MaterialInput{MaterialNo: m.MaterialNo, Grams: m.Grams})
	}
	if _, err := s.RegisterRecipe("r2", "R1", "v1", view.Name, mutated); !errors.Is(err, ErrRecipeExists) {
		t.Fatalf("改动内容再登记应返回 ErrRecipeExists，得到 %v", err)
	}
	// 原内容换请求编号再登记同样不可覆盖。
	if _, err := s.RegisterRecipe("r3", "R1", "v1", "原始配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); !errors.Is(err, ErrRecipeExists) {
		t.Fatalf("同内容换编号再登记应返回 ErrRecipeExists，得到 %v", err)
	}

	want := []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}
	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	checkRecipeView(t, got, "原始配方", want)
	batch, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	checkBatchRequirements(t, batch, []MaterialRequirement{
		{MaterialNo: "M1", RequiredGrams: "300", ActualGrams: "0", DifferenceGrams: "-300"},
		{MaterialNo: "M2", RequiredGrams: "1.5", ActualGrams: "0", DifferenceGrams: "-1.5"},
	})

	// 再发生一次正常写入后重新打开：本地改动不应随持久化混入台账。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	reopened, err := s2.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	checkRecipeView(t, reopened, "原始配方", want)
}
