package release

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) 失败: %v", dir, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func registerStandardRecipe(t *testing.T, s *Store, reqNo string) {
	t.Helper()
	_, err := s.RegisterRecipe(reqNo, "R1", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	})
	if err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
}

// 首次使用空位置：台账为空，查询返回未找到。
func TestOpenEmptyAndNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("查询不存在的批次应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("查询不存在的配方应返回 ErrNotFound，得到 %v", err)
	}
}

// 空位置关闭后重新打开，仍然为空台账。
func TestReopenEmptyLocation(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重新打开空位置应得到空台账，得到 %v", err)
	}
}

func TestRegisterRecipeAndGet(t *testing.T) {
	s := openTestStore(t)
	view, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.500"},
	})
	if err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
	if view.RecipeNo != "R1" || view.Version != "v1" || view.Name != "配方一" {
		t.Fatalf("配方视图不正确: %+v", view)
	}
	if len(view.Materials) != 2 || view.Materials[0].Grams != "100" || view.Materials[1].Grams != "0.5" {
		t.Fatalf("物料视图不正确: %+v", view.Materials)
	}

	got, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("查询配方失败: %v", err)
	}
	if got.Name != "配方一" || len(got.Materials) != 2 {
		t.Fatalf("查询结果不正确: %+v", got)
	}

	// 同一配方编号的不同版本可以登记。
	if _, err := s.RegisterRecipe("r2", "R1", "v2", "配方二", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("无物料登记应返回 ErrInvalidInput，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r2", "R1", "v2", "配方二", []MaterialInput{
		{MaterialNo: "M1", Grams: "2"},
	}); err != nil {
		t.Fatalf("登记不同版本失败: %v", err)
	}

	// 已登记版本不可覆盖。
	if _, err := s.RegisterRecipe("r3", "R1", "v1", "改名", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrRecipeExists) {
		t.Fatalf("覆盖已登记版本应返回 ErrRecipeExists，得到 %v", err)
	}
}

func TestRegisterRecipeValidation(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		name       string
		recipeNo   string
		version    string
		recipeName string
		materials  []MaterialInput
	}{
		{"空配方编号", "", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: "1"}}},
		{"空版本号", "R1", "", "n", []MaterialInput{{MaterialNo: "M1", Grams: "1"}}},
		{"空名称", "R1", "v1", "", []MaterialInput{{MaterialNo: "M1", Grams: "1"}}},
		{"无物料", "R1", "v1", "n", nil},
		{"物料编号为空", "R1", "v1", "n", []MaterialInput{{MaterialNo: "", Grams: "1"}}},
		{"物料编号重复", "R1", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: "1"}, {MaterialNo: "M1", Grams: "2"}}},
		{"克数为零", "R1", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: "0"}}},
		{"克数为负", "R1", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: "-1"}}},
		{"克数四位小数", "R1", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: "1.2345"}}},
		{"克数为空", "R1", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: ""}}},
		{"克数非法字符", "R1", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: "abc"}}},
		{"克数多位小数点", "R1", "v1", "n", []MaterialInput{{MaterialNo: "M1", Grams: "1.2.3"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.RegisterRecipe("req-"+tc.name, tc.recipeNo, tc.version, tc.recipeName, tc.materials)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("应返回 ErrInvalidInput，得到 %v", err)
			}
		})
	}
}

func TestCreateBatch(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")

	view, err := s.CreateBatch("b1", "B1", "R1", "v1", 10)
	if err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if view.BatchNo != "B1" || view.RecipeNo != "R1" || view.RecipeVersion != "v1" ||
		view.RecipeName != "标准配方" || view.PlannedPortions != 10 || view.Status != StatusDraft {
		t.Fatalf("批次视图不正确: %+v", view)
	}
	if len(view.Materials) != 3 {
		t.Fatalf("草稿也应列出全部配方物料，得到 %d 项", len(view.Materials))
	}

	// 批次编号重复。
	if _, err := s.CreateBatch("b2", "B1", "R1", "v1", 10); !errors.Is(err, ErrDuplicateBatch) {
		t.Fatalf("重复批次编号应返回 ErrDuplicateBatch，得到 %v", err)
	}
	// 配方版本不存在。
	if _, err := s.CreateBatch("b3", "B2", "R1", "v9", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的配方版本应返回 ErrNotFound，得到 %v", err)
	}
	// 份数不合法。
	for _, p := range []int{0, -1} {
		if _, err := s.CreateBatch(fmt.Sprintf("bp-%d", p), "B"+fmt.Sprint(p), "R1", "v1", p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("份数 %d 应返回 ErrInvalidInput，得到 %v", p, err)
		}
	}
}

func TestDraftUpdate(t *testing.T) {
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

	// 草稿可以改份数。
	view, err := s.UpdateDraftBatch("u1", "B1", "", "", 20)
	if err != nil {
		t.Fatalf("调整份数失败: %v", err)
	}
	if view.PlannedPortions != 20 {
		t.Fatalf("份数应为 20，得到 %d", view.PlannedPortions)
	}
	// 草稿可以改选配方版本。
	view, err = s.UpdateDraftBatch("u2", "B1", "R2", "v1", 0)
	if err != nil {
		t.Fatalf("改选配方失败: %v", err)
	}
	if view.RecipeNo != "R2" || view.RecipeVersion != "v1" {
		t.Fatalf("配方应已切换，得到 %+v", view)
	}
	// 只给配方编号不给版本号 → 非法。
	if _, err := s.UpdateDraftBatch("u3", "B1", "R1", "", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("只给配方编号应返回 ErrInvalidInput，得到 %v", err)
	}
	// 改到不存在的配方版本 → 未找到。
	if _, err := s.UpdateDraftBatch("u4", "B1", "R9", "v1", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("改到不存在的配方应返回 ErrNotFound，得到 %v", err)
	}
	// 份数非法。
	if _, err := s.UpdateDraftBatch("u5", "B1", "", "", 0); err != nil {
		t.Fatalf("传 0 应表示不改份数，得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("u6", "B1", "", "", -3); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("负份数应返回 ErrInvalidInput，得到 %v", err)
	}
	// 不存在的批次。
	if _, err := s.UpdateDraftBatch("u7", "B9", "", "", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("调整不存在的批次应返回 ErrNotFound，得到 %v", err)
	}

	// 开始执行后份数与配方固定。
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u8", "B1", "", "", 30); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("执行中调整应返回 ErrInvalidState，得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("u9", "B1", "R1", "v1", 0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("执行中改配方应返回 ErrInvalidState，得到 %v", err)
	}
	// 关闭后同样不能调整。
	if _, err := s.CloseBatch("c1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u10", "B1", "", "", 30); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("已关闭批次调整应返回 ErrInvalidState，得到 %v", err)
	}
}

func TestStartCloseLifecycle(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 只有草稿可以开始。
	if _, err := s.CloseBatch("c0", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("关闭草稿应返回 ErrInvalidState，得到 %v", err)
	}
	view, err := s.StartBatch("s1", "B1")
	if err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if view.Status != StatusExecuting {
		t.Fatalf("状态应为执行中，得到 %s", view.Status)
	}
	// 重复开始。
	if _, err := s.StartBatch("s2", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("重复开始应返回 ErrInvalidState，得到 %v", err)
	}
	// 只有执行中可以关闭。
	view, err = s.CloseBatch("c1", "B1")
	if err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if view.Status != StatusClosed {
		t.Fatalf("状态应为已关闭，得到 %s", view.Status)
	}
	// 关闭后不能再关、不能再开、不能投料、不能改草稿。
	if _, err := s.CloseBatch("c2", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("重复关闭应返回 ErrInvalidState，得到 %v", err)
	}
	if _, err := s.StartBatch("s3", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("关闭后重新打开应返回 ErrInvalidState，得到 %v", err)
	}
	if _, err := s.AddFeeding("f1", "B1", "M1", "10", time.Now(), "张三"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("关闭后投料应返回 ErrInvalidState，得到 %v", err)
	}
	// 不存在的批次。
	if _, err := s.StartBatch("s4", "B9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("开始不存在的批次应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := s.CloseBatch("c3", "B9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("关闭不存在的批次应返回 ErrNotFound，得到 %v", err)
	}
}

func TestAddFeeding(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 草稿状态不能投料。
	if _, err := s.AddFeeding("f0", "B1", "M1", "10", time.Now(), "张三"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("草稿投料应返回 ErrInvalidState，得到 %v", err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}

	// 物料必须属于配方版本。
	if _, err := s.AddFeeding("f-bad", "B1", "M9", "10", time.Now(), "张三"); !errors.Is(err, ErrMaterialNotInRecipe) {
		t.Fatalf("非配方物料应返回 ErrMaterialNotInRecipe，得到 %v", err)
	}
	// 克数非法。
	if _, err := s.AddFeeding("f-bad2", "B1", "M1", "0", time.Now(), "张三"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("零克数应返回 ErrInvalidInput，得到 %v", err)
	}
	if _, err := s.AddFeeding("f-bad3", "B1", "M1", "1.23456", time.Now(), "张三"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("超三位小数应返回 ErrInvalidInput，得到 %v", err)
	}
	// 登记人为空。
	if _, err := s.AddFeeding("f-bad4", "B1", "M1", "10", time.Now(), ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("登记人为空应返回 ErrInvalidInput，得到 %v", err)
	}
	// 不存在的批次。
	if _, err := s.AddFeeding("f-bad5", "B9", "M1", "10", time.Now(), "张三"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("向不存在批次投料应返回 ErrNotFound，得到 %v", err)
	}

	// 正常投料：故意把投料时间打乱，顺序仍按登记顺序。
	t1 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	f1, err := s.AddFeeding("f1", "B1", "M1", "100", t1, "张三")
	if err != nil {
		t.Fatalf("投料失败: %v", err)
	}
	if f1.Seq != 1 || f1.Grams != "100" || f1.Registrar != "张三" {
		t.Fatalf("投料视图不正确: %+v", f1)
	}
	if _, err := s.AddFeeding("f2", "B1", "M2", "3", t2, "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f3", "B1", "M1", "50.500", t3, "王五"); err != nil {
		t.Fatal(err)
	}

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 3 {
		t.Fatalf("应有 3 条投料，得到 %d", len(view.Feedings))
	}
	wantSeq := []int{1, 2, 3}
	wantMats := []string{"M1", "M2", "M1"}
	wantGrams := []string{"100", "3", "50.5"}
	for i, f := range view.Feedings {
		if f.Seq != wantSeq[i] || f.MaterialNo != wantMats[i] || f.Grams != wantGrams[i] {
			t.Fatalf("第 %d 条投料不正确: %+v", i, f)
		}
	}
}

// 数量核对：应投/实投/差额逐项计算，超投不抵消欠投，无投料也显示。
func TestBatchQueryComputation(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1") // M1=100, M2=0.5, M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// M1：应投 1000，先投 1000 再投 500（超投 500）。
	if _, err := s.AddFeeding("f1", "B1", "M1", "1000", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M1", "500", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	// M2：应投 5，投 3（欠 2）。M3 不投料。
	if _, err := s.AddFeeding("f3", "B1", "M2", "3", time.Now(), "李四"); err != nil {
		t.Fatal(err)
	}

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]MaterialRequirement{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m
	}
	check := func(mat, req, act, diff string) {
		t.Helper()
		m, ok := got[mat]
		if !ok {
			t.Fatalf("缺少物料 %q", mat)
		}
		if m.RequiredGrams != req || m.ActualGrams != act || m.DifferenceGrams != diff {
			t.Fatalf("物料 %q 核对应为 应投=%s 实投=%s 差额=%s，得到 %+v", mat, req, act, diff, m)
		}
	}
	check("M1", "1000", "1500", "500")
	check("M2", "5", "3", "-2")
	check("M3", "0.1", "0", "-0.1")
}

// 查询返回的是副本：调用方修改不影响台账。
func TestViewIsolation(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f1", "B1", "M1", "100", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	view.Status = StatusClosed
	view.PlannedPortions = 999
	view.Feedings[0].Grams = "9999"
	view.Materials[0].ActualGrams = "9999"
	view.Materials = append(view.Materials, MaterialRequirement{MaterialNo: "FAKE"})

	again, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != StatusExecuting || again.PlannedPortions != 10 {
		t.Fatalf("台账被查询结果修改: %+v", again)
	}
	if again.Feedings[0].Grams != "100" || again.Materials[0].ActualGrams != "100" {
		t.Fatalf("台账数据被查询结果修改: %+v", again)
	}
	if len(again.Materials) != 3 {
		t.Fatalf("物料项被修改，得到 %d 项", len(again.Materials))
	}
}

// 持久化：关闭后重新打开，配方、批次、投料、请求结果都在。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	fixedTime := time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC)
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("recipe-1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.AddFeeding("f1", "B1", "M1", "1000", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.AddFeeding("f2", "B1", "M2", "5", fixedTime, "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CloseBatch("c1", "B1"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	recipe, err := s2.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("重新打开后查询配方失败: %v", err)
	}
	if recipe.Name != "配方一" || len(recipe.Materials) != 2 {
		t.Fatalf("配方数据不完整: %+v", recipe)
	}
	batch, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatalf("重新打开后查询批次失败: %v", err)
	}
	if batch.Status != StatusClosed || batch.PlannedPortions != 10 || len(batch.Feedings) != 2 {
		t.Fatalf("批次数据不完整: %+v", batch)
	}
	if batch.Materials[0].ActualGrams != "1000" || batch.Materials[1].ActualGrams != "5" {
		t.Fatalf("投料数据不完整: %+v", batch.Materials)
	}

	// 请求结果也在：幂等重放（投料时间相同，内容一致）。
	replay, err := s2.AddFeeding("f1", "B1", "M1", "1000", fixedTime, "张三")
	if err != nil {
		t.Fatalf("重放投料请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，seq 应为 1，得到 %d", replay.Seq)
	}
	batch2, _ := s2.GetBatch("B1")
	if len(batch2.Feedings) != 2 {
		t.Fatalf("重放不应新增投料，得到 %d 条", len(batch2.Feedings))
	}
}

// 幂等：相同编号 + 相同操作 + 相同内容重复提交，返回第一次成功的结果，不重复产生业务变更。
func TestIdempotentReplay(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")

	b1, err := s.CreateBatch("b1", "B1", "R1", "v1", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 完全相同的请求重放。
	b2, err := s.CreateBatch("b1", "B1", "R1", "v1", 10)
	if err != nil {
		t.Fatalf("重放创建批次失败: %v", err)
	}
	if b2.Status != b1.Status || b2.BatchNo != b1.BatchNo {
		t.Fatalf("重放结果应与第一次一致: %+v vs %+v", b2, b1)
	}
	// 重放后批次仍然只有一个（通过查询验证数据未变）。
	got, err := s.GetBatch("B1")
	if err != nil || got.Status != StatusDraft {
		t.Fatalf("重放不应改变批次: %v %+v", err, got)
	}

	// 开始执行后重放创建请求，返回的仍是第一次成功时的草稿视图。
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	b3, err := s.CreateBatch("b1", "B1", "R1", "v1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if b3.Status != StatusDraft {
		t.Fatalf("重放应返回第一次成功的结果（草稿），得到状态 %s", b3.Status)
	}

	// 投料重放：即使批次已关闭，也返回原结果且不新增。
	fixedTime := time.Date(2026, 3, 4, 8, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("f1", "B1", "M1", "100", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("c1", "B1"); err != nil {
		t.Fatal(err)
	}
	f2, err := s.AddFeeding("f1", "B1", "M1", "100", fixedTime, "张三")
	if err != nil {
		t.Fatalf("关闭后重放投料失败: %v", err)
	}
	if f2.Seq != 1 || f2.Grams != "100" {
		t.Fatalf("重放投料结果不正确: %+v", f2)
	}
	got, _ = s.GetBatch("B1")
	if len(got.Feedings) != 1 {
		t.Fatalf("重放不应新增投料，得到 %d 条", len(got.Feedings))
	}
}

// 请求编号冲突：同一编号用于不同操作或不同内容必须拒绝。
func TestRequestConflict(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 同编号、不同内容（份数不同）。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 20); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同内容应返回 ErrRequestConflict，得到 %v", err)
	}
	// 同编号、不同操作。
	if _, err := s.StartBatch("b1", "B1"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同操作应返回 ErrRequestConflict，得到 %v", err)
	}
	if _, err := s.AddFeeding("b1", "B1", "M1", "10", time.Now(), "张三"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号不同操作应返回 ErrRequestConflict，得到 %v", err)
	}
	// 冲突拒绝后原数据不变。
	got, _ := s.GetBatch("B1")
	if got.Status != StatusDraft || got.PlannedPortions != 10 {
		t.Fatalf("冲突请求不应改变数据: %+v", got)
	}
}

// 失败不占用请求编号：被拒绝的请求编号可以在修正后用于成功提交。
func TestFailedRequestDoesNotConsumeNo(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")

	// 先用非法内容提交（份数 0），失败。
	if _, err := s.CreateBatch("x1", "B1", "R1", "v1", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("应失败，得到 %v", err)
	}
	// 同一编号用于合法内容，应成功。
	if _, err := s.CreateBatch("x1", "B1", "R1", "v1", 10); err != nil {
		t.Fatalf("失败不应占用编号，合法提交应成功: %v", err)
	}

	// 投料失败（非配方物料）后，同编号合法投料成功。
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("x2", "B1", "M9", "10", time.Now(), "张三"); !errors.Is(err, ErrMaterialNotInRecipe) {
		t.Fatalf("应失败，得到 %v", err)
	}
	if _, err := s.AddFeeding("x2", "B1", "M1", "10", time.Now(), "张三"); err != nil {
		t.Fatalf("失败不应占用编号，合法投料应成功: %v", err)
	}
	got, _ := s.GetBatch("B1")
	if len(got.Feedings) != 1 || got.Feedings[0].Seq != 1 {
		t.Fatalf("投料应成功登记一次: %+v", got.Feedings)
	}
}

// 并发：不同请求编号的投料全部生效，互不覆盖。
func TestConcurrentFeedings(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AddFeeding(fmt.Sprintf("f-%03d", i), "B1", "M1", "1", time.Now(),
				fmt.Sprintf("登记人%d", i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发投料失败: %v", err)
		}
	}

	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Feedings) != n {
		t.Fatalf("应有 %d 条投料，得到 %d", n, len(got.Feedings))
	}
	seqs := map[int]bool{}
	for _, f := range got.Feedings {
		if seqs[f.Seq] {
			t.Fatalf("投料序号重复: %d", f.Seq)
		}
		seqs[f.Seq] = true
	}
	if len(seqs) != n {
		t.Fatalf("投料序号应互不相同，得到 %d 个", len(seqs))
	}
	if got.Materials[0].ActualGrams != "50" {
		t.Fatalf("累计实投应为 50，得到 %s", got.Materials[0].ActualGrams)
	}
}

// 并发：同一请求同时到达，只产生一次业务变更。
func TestConcurrentSameRequest(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}

	const n = 20
	fixedTime := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.AddFeeding("same-req", "B1", "M1", "10", fixedTime, "张三")
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发相同请求应全部成功（重放），得到 %v", err)
		}
	}

	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Feedings) != 1 {
		t.Fatalf("同一请求只应产生一次投料，得到 %d 条", len(got.Feedings))
	}
	if got.Feedings[0].Grams != "10" {
		t.Fatalf("投料内容不正确: %+v", got.Feedings[0])
	}
}

// 不同数据位置互相独立。
func TestIndependentLocations(t *testing.T) {
	dir1 := filepath.Join(t.TempDir(), "loc1")
	dir2 := filepath.Join(t.TempDir(), "loc2")

	s1, err := Open(dir1)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	defer s2.Close()

	if _, err := s1.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	if _, err := s2.GetRecipe("R1", "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("位置 2 不应看到位置 1 的配方，得到 %v", err)
	}
	if _, err := s2.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("位置 2 不应看到位置 1 的批次，得到 %v", err)
	}

	// 两个位置可以各自使用相同的请求编号而互不影响。
	if _, err := s2.RegisterRecipe("r1", "R1", "v1", "另一个配方", []MaterialInput{
		{MaterialNo: "M2", Grams: "2"},
	}); err != nil {
		t.Fatalf("不同位置应独立登记: %v", err)
	}
	r1, _ := s1.GetRecipe("R1", "v1")
	r2, _ := s2.GetRecipe("R1", "v1")
	if r1.Name == r2.Name {
		t.Fatalf("两个位置的同名配方不应互相影响")
	}
}

// 已有数据损坏时明确报错，不能当成空台账继续。
func TestCorruptData(t *testing.T) {
	dir := t.TempDir()
	// 先写入有效数据。
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// 破坏台账文件。
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte("{不是合法JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏数据应返回 ErrCorruptData，得到 %v", err)
	}

	// 合法 JSON 但版本不受支持。
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(`{"version":99,"recipes":[],"batches":[],"requests":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("不支持的版本应返回 ErrCorruptData，得到 %v", err)
	}

	// 空文件视为空台账（首次使用），可以正常打开。
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("空文件应视为空台账: %v", err)
	}
}

// 跨进程并发：两个进程同时向同一台账投料，数据不互相覆盖。
// 本子进程由 TestCrossProcessConcurrent 通过环境变量驱动。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("HELPER_PROCESS") != "1" {
		return
	}
	dir := os.Getenv("HELPER_DIR")
	id := os.Getenv("HELPER_ID")
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper open: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()
	for i := 0; i < 20; i++ {
		if _, err := s.AddFeeding(fmt.Sprintf("f-%s-%03d", id, i), "B1", "M1", "1",
			time.Now(), "登记人"+id); err != nil {
			fmt.Fprintf(os.Stderr, "helper feed: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func TestCrossProcessConcurrent(t *testing.T) {
	dir := t.TempDir()
	// 父进程先登记配方，避免与子进程的登记请求竞争同一版本。
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("recipe-parent", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b-parent", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s-parent", "B1"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	runHelper := func(id string) error {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(),
			"HELPER_PROCESS=1",
			"HELPER_DIR="+dir,
			"HELPER_ID="+id,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"A", "B"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- runHelper(id)
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("子进程失败: %v", err)
		}
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Feedings) != 40 {
		t.Fatalf("两个进程共应登记 40 条投料，得到 %d 条（数据被覆盖）", len(got.Feedings))
	}
	seqs := map[int]bool{}
	for _, f := range got.Feedings {
		if seqs[f.Seq] {
			t.Fatalf("投料序号重复: %d", f.Seq)
		}
		seqs[f.Seq] = true
	}
	if got.Materials[0].ActualGrams != "40" {
		t.Fatalf("累计实投应为 40，得到 %s", got.Materials[0].ActualGrams)
	}
}

// 累计实投上限：同一批次内每种物料累计上限 9223372036854775.807 克，
// 按物料分别判断；恰好等于上限允许，超过返回 ErrInvalidInput。
func TestAddFeedingCumulativeCap(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1") // M1=100, M2=0.5, M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}

	const cap = "9223372036854775.807"

	// 已累计 9223372036854775.806，再登记 0.001 恰好等于上限，应成功。
	if _, err := s.AddFeeding("f1", "B1", "M1", "9223372036854775.806", time.Now(), "张三"); err != nil {
		t.Fatalf("累计未超过上限应成功，得到 %v", err)
	}
	f2, err := s.AddFeeding("f2", "B1", "M1", "0.001", time.Now(), "张三")
	if err != nil {
		t.Fatalf("累计恰好等于上限应成功，得到 %v", err)
	}
	if f2.Seq != 2 {
		t.Fatalf("成功登记序号应为 2，得到 %d", f2.Seq)
	}

	// 再登记 0.001 会超过上限，必须失败并指出物料。
	_, err = s.AddFeeding("f3", "B1", "M1", "0.001", time.Now(), "张三")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("累计超过上限应返回 ErrInvalidInput，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "M1") {
		t.Fatalf("错误信息应指出是哪种物料，得到 %v", err)
	}

	// 单次输入本身超过上限也拒绝，不能回绕成较小的合法克数后接受。
	for _, g := range []string{"9223372036854775.808", "18446744073709551.616", "9999999999999999999"} {
		if _, err := s.AddFeeding("f-big-"+g, "B1", "M2", g, time.Now(), "张三"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("克数 %s 超过上限应返回 ErrInvalidInput，得到 %v", g, err)
		}
	}

	// 上限按物料分别判断：M1 已满不影响其他物料；M2 投到上限应成功。
	if _, err := s.AddFeeding("f4", "B1", "M2", cap, time.Now(), "李四"); err != nil {
		t.Fatalf("其他物料累计不受 M1 影响，应成功，得到 %v", err)
	}
	// M2 也满后，再投最小精度即失败。
	if _, err := s.AddFeeding("f5", "B1", "M2", "0.001", time.Now(), "李四"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("M2 累计超过上限应返回 ErrInvalidInput，得到 %v", err)
	}

	// 被拒绝的请求不占用编号、不推进序号、不改变状态：修正数量后仍可提交。
	if _, err := s.AddFeeding("f5", "B1", "M3", "0.001", time.Now(), "王五"); err != nil {
		t.Fatalf("失败不应占用请求编号，合法提交应成功: %v", err)
	}

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	// 成功的投料只有：M1 两条、M2 一条、M3 一条。
	if len(view.Feedings) != 4 {
		t.Fatalf("被拒绝的投料不应保存，应只有 4 条，得到 %d 条: %+v", len(view.Feedings), view.Feedings)
	}
	if view.Feedings[3].Seq != 4 || view.Feedings[3].MaterialNo != "M3" {
		t.Fatalf("被拒绝的登记不应占用序号，最后一条应为 seq=4 M3，得到 %+v", view.Feedings[3])
	}
	if view.Status != StatusExecuting {
		t.Fatalf("被拒绝的投料不应改变批次状态，得到 %s", view.Status)
	}

	got := map[string]MaterialRequirement{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m
	}
	// 成功登记的克数必须完整计入核对结果，实投与差额准确、不为负。
	if got["M1"].ActualGrams != cap {
		t.Fatalf("M1 累计实投应为上限 %s，得到 %s", cap, got["M1"].ActualGrams)
	}
	if got["M1"].DifferenceGrams != "9223372036853775.807" {
		t.Fatalf("M1 差额（实投减应投 1000）不正确: %s", got["M1"].DifferenceGrams)
	}
	if got["M2"].ActualGrams != cap {
		t.Fatalf("M2 累计实投应为上限 %s，得到 %s", cap, got["M2"].ActualGrams)
	}
	if got["M3"].ActualGrams != "0.001" || got["M3"].DifferenceGrams != "-0.099" {
		t.Fatalf("M3 核对不正确: %+v", got["M3"])
	}
}

// 克数精度：三位小数精确计算，不丢精度。
func TestGramsPrecision(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "精度配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.001"},
		{MaterialNo: "M2", Grams: "100.123"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// 0.001 + 0.002 = 0.003（浮点下 0.001+0.002 != 0.003）。
	if _, err := s.AddFeeding("f1", "B1", "M1", "0.001", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M1", "0.002", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]MaterialRequirement{}
	for _, mm := range got.Materials {
		m[mm.MaterialNo] = mm
	}
	if m["M1"].RequiredGrams != "0.003" || m["M1"].ActualGrams != "0.003" || m["M1"].DifferenceGrams != "0" {
		t.Fatalf("M1 数量核对不正确: %+v", m["M1"])
	}
	if m["M2"].RequiredGrams != "300.369" {
		t.Fatalf("M2 应投量不正确: %+v", m["M2"])
	}
}
