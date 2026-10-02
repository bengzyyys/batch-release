package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func mustGrams(t *testing.T, s string) Grams {
	t.Helper()
	g, err := ParseGrams(s)
	if err != nil {
		t.Fatalf("ParseGrams(%q): %v", s, err)
	}
	return g
}

func openTemp(t *testing.T) (*Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l, dir
}

func sampleRecipe(t *testing.T) RecipeVersion {
	t.Helper()
	return RecipeVersion{
		RecipeID: "R-1",
		Version:  "v1",
		Name:     "标准配方",
		Materials: []Material{
			{MaterialID: "M-A", Grams: mustGrams(t, "2.5")},
			{MaterialID: "M-B", Grams: mustGrams(t, "0.125")},
		},
	}
}

func registerSample(t *testing.T, l *Ledger) {
	t.Helper()
	if err := l.RegisterRecipe("req-recipe-1", sampleRecipe(t)); err != nil {
		t.Fatalf("RegisterRecipe: %v", err)
	}
}

func TestParseGrams(t *testing.T) {
	cases := map[string]Grams{
		"1":       1000,
		"2.5":     2500,
		"0.001":   1,
		"12.500":  12500,
		"0.1":     100,
		"1000000": 1000000000,
	}
	for in, want := range cases {
		got, err := ParseGrams(in)
		if err != nil {
			t.Errorf("ParseGrams(%q) 出错: %v", in, err)
		} else if got != want {
			t.Errorf("ParseGrams(%q) = %d, 期望 %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "0", "0.0", "-1", "1.0001", "1.2345", "abc", "1.", ".5", "1e3"} {
		if _, err := ParseGrams(bad); !errors.Is(err, ErrInvalidQuantity) {
			t.Errorf("ParseGrams(%q) 应返回 ErrInvalidQuantity，得到 %v", bad, err)
		}
	}
	if got := mustGrams(t, "12.500").String(); got != "12.5" {
		t.Errorf("String() = %q, 期望 12.5", got)
	}
}

func TestEmptyLocationGivesEmptyLedger(t *testing.T) {
	l, _ := openTemp(t)
	if _, err := l.GetBatch("B-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空台账查询应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := l.GetRecipe("R-1", "v1"); !errors.Is(err, ErrRecipeNotFound) {
		t.Fatalf("空台账查询配方应返回 ErrRecipeNotFound，得到 %v", err)
	}
}

func TestRegisterRecipeValidation(t *testing.T) {
	l, _ := openTemp(t)
	rv := sampleRecipe(t)

	dup := rv
	dup.Materials = []Material{{MaterialID: "M-A", Grams: 1}, {MaterialID: "M-A", Grams: 2}}
	if err := l.RegisterRecipe("r1", dup); !errors.Is(err, ErrDuplicateMaterial) {
		t.Errorf("重复物料应返回 ErrDuplicateMaterial，得到 %v", err)
	}

	empty := rv
	empty.Materials = nil
	if err := l.RegisterRecipe("r2", empty); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("无物料应返回 ErrInvalidInput，得到 %v", err)
	}

	zero := rv
	zero.Materials = []Material{{MaterialID: "M-A", Grams: 0}}
	if err := l.RegisterRecipe("r3", zero); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("零克数应返回 ErrInvalidQuantity，得到 %v", err)
	}

	// 失败不占用请求编号：修正内容后同一编号可以成功。
	if err := l.RegisterRecipe("r3", rv); err != nil {
		t.Errorf("失败后的请求编号应可重用: %v", err)
	}
	// 已登记版本不可覆盖。
	changed := rv
	changed.Name = "改名"
	if err := l.RegisterRecipe("r4", changed); !errors.Is(err, ErrRecipeExists) {
		t.Errorf("重复登记同一版本应返回 ErrRecipeExists，得到 %v", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	l, dir := openTemp(t)
	registerSample(t, l)
	if err := l.CreateBatch("c1", "B-1", "R-1", "v1", 4); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if err := l.StartBatch("s1", "B-1"); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	fedAt := time.Date(2026, 10, 3, 8, 30, 0, 0, time.UTC)
	if err := l.AddFeeding("f1", "B-1", "M-A", mustGrams(t, "3.25"), fedAt, "张三"); err != nil {
		t.Fatalf("AddFeeding: %v", err)
	}

	l2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	rep, err := l2.GetBatch("B-1")
	if err != nil {
		t.Fatalf("重开后查询: %v", err)
	}
	if rep.Status != StatusRunning || rep.Units != 4 || rep.RecipeName != "标准配方" {
		t.Errorf("重开后批次内容不符: %+v", rep)
	}
	if len(rep.Feedings) != 1 || rep.Feedings[0].Grams != 3250 || rep.Feedings[0].Operator != "张三" {
		t.Errorf("重开后投料不符: %+v", rep.Feedings)
	}
	// 重开后请求编号仍然幂等。
	if err := l2.AddFeeding("f1", "B-1", "M-A", mustGrams(t, "3.25"), fedAt, "张三"); err != nil {
		t.Errorf("重开后重放同一请求应成功: %v", err)
	}
	rep, _ = l2.GetBatch("B-1")
	if len(rep.Feedings) != 1 {
		t.Errorf("重放不应重复投料，现有 %d 条", len(rep.Feedings))
	}
}

func TestCorruptDataFailsOpen(t *testing.T) {
	l, dir := openTemp(t)
	registerSample(t, l)
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte("{不是合法数据"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("数据损坏应返回 ErrCorruptData，得到 %v", err)
	}
	// 数据位置不可创建也应明确报错。
	if _, err := Open(filepath.Join(dir, "ledger.json", "sub")); err == nil {
		t.Fatal("非法数据位置应报错")
	}
}

func TestLocationsAreIndependent(t *testing.T) {
	l1, _ := openTemp(t)
	l2, _ := openTemp(t)
	registerSample(t, l1)
	if _, err := l2.GetRecipe("R-1", "v1"); !errors.Is(err, ErrRecipeNotFound) {
		t.Fatalf("另一位置不应看到配方，得到 %v", err)
	}
}

func TestBatchLifecycle(t *testing.T) {
	l, _ := openTemp(t)
	registerSample(t, l)
	rv2 := sampleRecipe(t)
	rv2.Version = "v2"
	rv2.Name = "改进配方"
	if err := l.RegisterRecipe("r2", rv2); err != nil {
		t.Fatalf("RegisterRecipe v2: %v", err)
	}

	if err := l.CreateBatch("c1", "B-1", "R-1", "v9", 1); !errors.Is(err, ErrRecipeNotFound) {
		t.Fatalf("未登记配方版本应返回 ErrRecipeNotFound，得到 %v", err)
	}
	if err := l.CreateBatch("c2", "B-1", "R-1", "v1", 0); !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("份数为 0 应返回 ErrInvalidQuantity，得到 %v", err)
	}
	if err := l.CreateBatch("c3", "B-1", "R-1", "v1", 10); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if err := l.CreateBatch("c4", "B-1", "R-1", "v1", 1); !errors.Is(err, ErrBatchExists) {
		t.Fatalf("批次编号重复应返回 ErrBatchExists，得到 %v", err)
	}

	// 草稿可调整份数和改选版本。
	if err := l.UpdateDraft("u1", "B-1", "R-1", "v2", 5); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	rep, _ := l.GetBatch("B-1")
	if rep.Version != "v2" || rep.Units != 5 || rep.Status != StatusDraft {
		t.Errorf("草稿调整未生效: %+v", rep)
	}

	// 草稿不能关闭、不能投料。
	if err := l.CloseBatch("x1", "B-1"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("草稿关闭应返回 ErrInvalidState，得到 %v", err)
	}
	if err := l.AddFeeding("x2", "B-1", "M-A", 1000, time.Now(), "张三"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("草稿投料应返回 ErrInvalidState，得到 %v", err)
	}

	if err := l.StartBatch("s1", "B-1"); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	// 开始后份数和配方固定。
	if err := l.UpdateDraft("u2", "B-1", "R-1", "v1", 3); !errors.Is(err, ErrInvalidState) {
		t.Errorf("执行中调整应返回 ErrInvalidState，得到 %v", err)
	}
	if err := l.StartBatch("s2", "B-1"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("重复开始应返回 ErrInvalidState，得到 %v", err)
	}

	if err := l.CloseBatch("cl1", "B-1"); err != nil {
		t.Fatalf("CloseBatch: %v", err)
	}
	// 关闭后不能投料、不能重开、不能再关闭。
	if err := l.AddFeeding("x3", "B-1", "M-A", 1000, time.Now(), "张三"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("关闭后投料应返回 ErrInvalidState，得到 %v", err)
	}
	if err := l.StartBatch("x4", "B-1"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("关闭后重开应返回 ErrInvalidState，得到 %v", err)
	}
	if err := l.CloseBatch("x5", "B-1"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("重复关闭应返回 ErrInvalidState，得到 %v", err)
	}
	// 失败操作不改变状态。
	rep, _ = l.GetBatch("B-1")
	if rep.Status != StatusClosed || rep.Units != 5 || rep.Version != "v2" {
		t.Errorf("失败操作后状态被改变: %+v", rep)
	}
}

func TestFeedingAndReport(t *testing.T) {
	l, _ := openTemp(t)
	registerSample(t, l)
	if err := l.CreateBatch("c1", "B-1", "R-1", "v1", 4); err != nil {
		t.Fatal(err)
	}
	if err := l.StartBatch("s1", "B-1"); err != nil {
		t.Fatal(err)
	}

	// 物料必须属于绑定的配方版本。
	if err := l.AddFeeding("f0", "B-1", "M-X", 1000, time.Now(), "张三"); !errors.Is(err, ErrUnknownMaterial) {
		t.Fatalf("非配方物料应返回 ErrUnknownMaterial，得到 %v", err)
	}
	if err := l.AddFeeding("f0b", "B-1", "M-A", 0, time.Now(), "张三"); !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("零投料应返回 ErrInvalidQuantity，得到 %v", err)
	}

	// 故意用乱序的投料时间，验证按登记顺序而非投料时间展示。
	t1 := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC) // 更早，但后登记
	if err := l.AddFeeding("f1", "B-1", "M-A", mustGrams(t, "6"), t1, "张三"); err != nil {
		t.Fatal(err)
	}
	if err := l.AddFeeding("f2", "B-1", "M-A", mustGrams(t, "5"), t2, "李四"); err != nil {
		t.Fatal(err)
	}
	// M-B 不投，验证欠投不被 M-A 超投抵消（M-A 应投 10，实投 11，超 1）。

	rep, err := l.GetBatch("B-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Feedings) != 2 || rep.Feedings[0].Operator != "张三" || rep.Feedings[1].Operator != "李四" {
		t.Errorf("投料应按登记顺序展示: %+v", rep.Feedings)
	}
	if len(rep.Materials) != 2 {
		t.Fatalf("没有投料的物料也要显示: %+v", rep.Materials)
	}
	ma := rep.Materials[0]
	if ma.MaterialID != "M-A" || ma.Required != 10000 || ma.Actual != 11000 || ma.Diff != 1000 {
		t.Errorf("M-A 核对不符: %+v", ma)
	}
	mb := rep.Materials[1]
	if mb.MaterialID != "M-B" || mb.Required != 500 || mb.Actual != 0 || mb.Diff != -500 {
		t.Errorf("M-B 欠投不应被抵消: %+v", mb)
	}
}

func TestQueryResultIsCopy(t *testing.T) {
	l, _ := openTemp(t)
	registerSample(t, l)
	if err := l.CreateBatch("c1", "B-1", "R-1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	rep, _ := l.GetBatch("B-1")
	rep.Units = 999
	rep.Status = StatusClosed
	rep.Materials[0].Actual = 999
	rep.Feedings = append(rep.Feedings, Feeding{MaterialID: "M-A", Grams: 1})
	again, _ := l.GetBatch("B-1")
	if again.Units != 2 || again.Status != StatusDraft || again.Materials[0].Actual != 0 || len(again.Feedings) != 0 {
		t.Errorf("调用方修改查询结果影响了台账: %+v", again)
	}
}

func TestIdempotentRequests(t *testing.T) {
	l, _ := openTemp(t)
	registerSample(t, l)

	if err := l.CreateBatch("req-1", "B-1", "R-1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	// 同编号同内容：返回首次成功结果，不新增批次。
	if err := l.CreateBatch("req-1", "B-1", "R-1", "v1", 3); err != nil {
		t.Errorf("重放创建应成功: %v", err)
	}
	// 同编号不同内容：拒绝。
	if err := l.CreateBatch("req-1", "B-2", "R-1", "v1", 3); !errors.Is(err, ErrRequestConflict) {
		t.Errorf("同编号不同内容应返回 ErrRequestConflict，得到 %v", err)
	}
	if err := l.StartBatch("req-1", "B-1"); !errors.Is(err, ErrRequestConflict) {
		t.Errorf("同编号另一操作应返回 ErrRequestConflict，得到 %v", err)
	}
	if _, err := l.GetBatch("B-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("被拒绝的请求不应留下记录，得到 %v", err)
	}

	if err := l.StartBatch("req-2", "B-1"); err != nil {
		t.Fatal(err)
	}
	fedAt := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	if err := l.AddFeeding("req-3", "B-1", "M-A", 1000, fedAt, "张三"); err != nil {
		t.Fatal(err)
	}
	if err := l.CloseBatch("req-4", "B-1"); err != nil {
		t.Fatal(err)
	}
	// 批次关闭后重放投料请求：仍返回第一次成功的结果，不重复投料。
	if err := l.AddFeeding("req-3", "B-1", "M-A", 1000, fedAt, "张三"); err != nil {
		t.Errorf("关闭后重放应返回首次成功结果: %v", err)
	}
	rep, _ := l.GetBatch("B-1")
	if len(rep.Feedings) != 1 {
		t.Errorf("重放不应重复投料，现有 %d 条", len(rep.Feedings))
	}

	// 失败不占用编号：先用 req-9 做一次非法投料，再用同编号做合法投料。
	l2, _ := openTemp(t)
	registerSample(t, l2)
	if err := l2.CreateBatch("c1", "B-9", "R-1", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if err := l2.AddFeeding("req-9", "B-9", "M-A", 1000, fedAt, "张三"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("草稿投料应失败，得到 %v", err)
	}
	if err := l2.StartBatch("req-8", "B-9"); err != nil {
		t.Fatal(err)
	}
	if err := l2.AddFeeding("req-9", "B-9", "M-A", 1000, fedAt, "张三"); err != nil {
		t.Errorf("失败请求不应占用编号: %v", err)
	}
}

func TestConcurrentSameRequest(t *testing.T) {
	l, _ := openTemp(t)
	registerSample(t, l)
	if err := l.CreateBatch("c1", "B-1", "R-1", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if err := l.StartBatch("s1", "B-1"); err != nil {
		t.Fatal(err)
	}
	fedAt := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = l.AddFeeding("same-req", "B-1", "M-A", 1000, fedAt, "张三")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("并发重放 %d 应全部成功: %v", i, err)
		}
	}
	rep, _ := l.GetBatch("B-1")
	if len(rep.Feedings) != 1 {
		t.Errorf("同一请求并发到达只能产生一次业务变更，现有 %d 条投料", len(rep.Feedings))
	}
}

func TestConcurrentDistinctWrites(t *testing.T) {
	l, dir := openTemp(t)
	registerSample(t, l)
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = l.CreateBatch(fmt.Sprintf("req-%d", i), fmt.Sprintf("B-%d", i), "R-1", "v1", i+1)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("并发创建 %d 失败: %v", i, err)
		}
	}
	// 重开后所有批次都在，互不覆盖。
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range errs {
		rep, err := l2.GetBatch(fmt.Sprintf("B-%d", i))
		if err != nil {
			t.Errorf("批次 B-%d 丢失: %v", i, err)
		} else if rep.Units != i+1 {
			t.Errorf("批次 B-%d 份数被覆盖: %d", i, rep.Units)
		}
	}
}
