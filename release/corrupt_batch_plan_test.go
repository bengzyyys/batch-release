package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 构造只有一个批次的台账：配方每份克数与批次份数、状态可指定。
func writeBatchPlanState(t *testing.T, dir string, grams gramsMilli, portions int, status BatchStatus) []byte {
	t.Helper()
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: grams}},
		}},
		Batches: []*batchRecord{{
			BatchNo:         "B1",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: portions,
			Status:          status,
		}},
	})
}

// 已保存批次的计划份数为零或负数时，Open 必须返回 ErrCorruptData：
// 草稿、执行中、已关闭三种状态一视同仁；错误信息指出批次编号与计划份数，
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsNonPositivePlannedPortions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		portions int
	}{
		{"零", 0},
		{"负数", -7},
	} {
		for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
			t.Run(tc.name+"/"+string(status), func(t *testing.T) {
				dir := t.TempDir()
				original := writeBatchPlanState(t, dir, 100000, tc.portions, status)

				s, err := Open(dir)
				if !errors.Is(err, ErrCorruptData) {
					t.Fatalf("计划份数非正应返回 ErrCorruptData，得到 %v", err)
				}
				if s != nil {
					s.Close()
					t.Fatalf("损坏台账不应返回可用的 Store 对象")
				}
				msg := err.Error()
				for _, want := range []string{"B1", "正整数"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
					}
				}
				got, err := os.ReadFile(filepath.Join(dir, stateFileName))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("拒绝打开不应改动台账文件")
				}
			})
		}
	}
}

// 某物料每份需要恰好上限数量：一份合法，两份必须拒绝——
// 错误信息要指出批次编号、计划份数、对应物料及绑定的配方编号、版本号。
func TestOpenRequiredGramsBoundary(t *testing.T) {
	// 一份：M1 每份即上限，应投恰好等于上限，正常读取。
	dir := t.TempDir()
	writeBatchPlanState(t, dir, gramsMilli(math.MaxInt64), 1, StatusExecuting)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("应投恰好等于上限的一份应正常打开: %v", err)
	}
	view, err := s.GetBatch("B1")
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	var m1 MaterialRequirement
	for _, m := range view.Materials {
		if m.MaterialNo == "M1" {
			m1 = m
		}
	}
	if m1.RequiredGrams != limitGrams {
		s.Close()
		t.Fatalf("一份的应投应为上限 %s，得到 %s", limitGrams, m1.RequiredGrams)
	}
	s.Close()

	// 两份：上限 × 2 溢出，Open 必须失败并给出完整定位信息。
	dir = t.TempDir()
	original := writeBatchPlanState(t, dir, gramsMilli(math.MaxInt64), 2, StatusExecuting)
	s, err = Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("应投超限应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"B1", "2", "M1", "R1", "v1", "应投", "上限"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 上限按每种物料分别判断：两种物料各自应投接近上限、合计远超上限仍合法；
// 只有单种物料的每份克数 × 份数本身超限才拒绝。
func TestOpenRequiredGramsPerMaterialNotSummed(t *testing.T) {
	dir := t.TempDir()
	// 每种物料每份 5e18 毫微克（约为上限的 54%），一份；
	// 两种物料合计 1e19 > 上限 9.22e18，但单项都不超限。
	const each gramsMilli = 5_000_000_000_000_000_000
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: each},
				{MaterialNo: "M2", GramsMilli: each},
			},
		}},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusExecuting,
		}},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("各物料单项不超限、合计超限时不应拒绝，得到 %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m.RequiredGrams
	}
	if got["M1"] != each.String() || got["M2"] != each.String() {
		t.Fatalf("两种物料的应投应各自保留，得到 %v", got)
	}

	// 把份数改成 2：每种物料单项即 1e18*... 超过上限，必须拒绝，
	// 不能用“两项平均”或合计之外的任何方式放行。
	dir2 := t.TempDir()
	writeStateFile(t, dir2, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: each},
				{MaterialNo: "M2", GramsMilli: each},
			},
		}},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusExecuting,
		}},
	})
	if _, err := Open(dir2); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("单种物料应投超限应返回 ErrCorruptData，得到 %v", err)
	}
}

// 应投量按批次实际绑定的配方版本计算：同编号另一版本是否超限不作数，
// 不能自动改用其他版本放行或顶罪。
func TestOpenRequiredGramsUsesBoundVersion(t *testing.T) {
	build := func(t *testing.T, dir string, boundVersion string, portions int) {
		t.Helper()
		writeStateFile(t, dir, &persistedState{
			Version: stateVersion,
			Recipes: []*recipeRecord{
				{
					RecipeNo: "R1", Version: "v1", Name: "小份配方",
					Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}},
				},
				{
					RecipeNo: "R1", Version: "v2", Name: "上限配方",
					Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: gramsMilli(math.MaxInt64)}},
				},
			},
			Batches: []*batchRecord{{
				BatchNo: "B1", RecipeNo: "R1", RecipeVersion: boundVersion,
				PlannedPortions: portions, Status: StatusExecuting,
			}},
		})
	}

	// 绑定 v1（每份 100 克）、两份：v2 每份即上限与本批次无关，必须正常读取。
	dir := t.TempDir()
	build(t, dir, "v1", 2)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("应按绑定的 v1 计算应投，v2 超限不影响本批次: %v", err)
	}
	s.Close()

	// 绑定 v2（每份即上限）、两份：即使 v1 的两份完全合法，也必须按 v2 拒绝。
	dir = t.TempDir()
	build(t, dir, "v2", 2)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("应按绑定的 v2 计算并拒绝两份，得到 %v", err)
	}
}

// 整份台账中只要一个批次份数非法或应投超限，其他批次再完整也不能放行；
// 修正问题批次的份数后重新打开，全部批次（含各自状态与份数）继续可用。
func TestOpenOneBadPlanAmongCompleteBatches(t *testing.T) {
	mkState := func(badPortions int) *persistedState {
		return &persistedState{
			Version: stateVersion,
			Recipes: []*recipeRecord{
				{
					RecipeNo: "R1", Version: "v1", Name: "普通配方",
					Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}},
				},
				{
					RecipeNo: "R9", Version: "v1", Name: "上限配方",
					Materials: []materialRecord{{MaterialNo: "Mbig", GramsMilli: gramsMilli(math.MaxInt64)}},
				},
			},
			Batches: []*batchRecord{
				{BatchNo: "B-ok-1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusDraft},
				{BatchNo: "B-bad", RecipeNo: "R9", RecipeVersion: "v1", PlannedPortions: badPortions, Status: StatusExecuting},
				{BatchNo: "B-ok-2", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 3, Status: StatusClosed},
			},
		}
	}

	dir := t.TempDir()
	writeStateFile(t, dir, mkState(2)) // B-bad 两份：Mbig 应投超限
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("一个批次应投超限应整体拒绝，得到 %v", err)
	}

	// 把问题批次改成一份（应投恰好等于上限），其余记录原样保留。
	writeStateFile(t, dir, mkState(1))
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("修正份数后重新打开应成功: %v", err)
	}
	defer s.Close()
	for _, want := range []struct {
		no       string
		version  string
		portions int
		status   BatchStatus
	}{
		{"B-ok-1", "v1", 1, StatusDraft},
		{"B-bad", "v1", 1, StatusExecuting},
		{"B-ok-2", "v1", 3, StatusClosed},
	} {
		b, err := s.GetBatch(want.no)
		if err != nil {
			t.Fatalf("修正后查询批次 %q 失败: %v", want.no, err)
		}
		if b.RecipeVersion != want.version || b.PlannedPortions != want.portions || b.Status != want.status {
			t.Fatalf("批次 %q 原有内容应保留，得到 %+v", want.no, b)
		}
	}
}

// 计划合法但尚未投料、实投不足或超出应投的批次仍可查询；
// 执行中批次仍可按原规则关闭，不新增数量吻合要求——
// 即使应投量恰好等于可表示上限、实投远远不足也一样。
func TestOpenFeedingMismatchStillReadableAndClosable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: limitGrams},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// 实投 1 克，应投为上限：严重欠投，不构成损坏。
	if _, err := s.AddFeeding("f1", "B1", "Mbig", "1", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	// 对 M2 超投：应投 1 克，实投 5 克，同样不构成损坏。
	if _, err := s.AddFeeding("f2", "B1", "M2", "5", time.Now(), "李四"); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("欠投/超投的合法计划仍应可查询: %v", err)
	}
	got := map[string]MaterialRequirement{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m
	}
	if got["Mbig"].RequiredGrams != limitGrams || got["Mbig"].ActualGrams != "1" {
		t.Fatalf("Mbig 数量核对不正确: %+v", got["Mbig"])
	}
	if got["M2"].RequiredGrams != "1" || got["M2"].ActualGrams != "5" || got["M2"].DifferenceGrams != "4" {
		t.Fatalf("M2 超投核对不正确: %+v", got["M2"])
	}
	// 数量不吻合仍可按原规则关闭。
	closed, err := s.CloseBatch("c1", "B1")
	if err != nil {
		t.Fatalf("数量不吻合不应阻止关闭: %v", err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("关闭后状态不正确: %s", closed.Status)
	}

	// 重开台账：欠投/超投记录与上限应投量原样可读。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("含欠投/超投记录的台账重开应成功: %v", err)
	}
	defer s2.Close()
	again, err := s2.GetBatch("B1")
	if err != nil || again.Status != StatusClosed {
		t.Fatalf("重开后批次查询失败或状态丢失: %v %+v", err, again)
	}
}

// 台账打开后批次计划被改成非法内容（份数清零）：下一次查询或写入都必须
// 返回 ErrCorruptData，即使操作的是另一个正常批次或配方；被拒绝的操作
// 不修改文件、不占用请求编号；把份数恢复为合法值后原记录继续可用，
// 被拒绝过的请求编号仍可合法提交。
func TestPlanCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}

	// 把 B1 的计划份数改成 0（B2 仍完整）。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	for _, b := range broken.Batches {
		if b.BatchNo == "B1" {
			b.PlannedPortions = 0
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询损坏批次与正常批次、查询配方，全部失败。
	for _, batchNo := range []string{"B1", "B2"} {
		if _, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询配方也应失败，得到 %v", err)
	}

	// 写入：对正常批次投料、登记新配方、调整其他草稿，全部不能绕过。
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r3", "R3", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方也应失败，得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("upd-b2", "B2", "", "", 9); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整草稿也应失败，得到 %v", err)
	}

	// 被拒绝的操作不改文件、不留请求记录。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的操作不应改动台账文件")
	}
	if bytes.Contains(after, []byte("feed-rejected")) || bytes.Contains(after, []byte("R3")) {
		t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
	}

	// 恢复合法份数后原记录完整可用，被拒绝过的请求编号仍可合法提交。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if b2.PlannedPortions != 5 || b2.Status != StatusExecuting {
		t.Fatalf("正常批次原有内容应保留: %+v", b2)
	}
	f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 1 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 对已保存数据的份数检查不能误伤合法调用：草稿调整时份数传 0 仍表示
// 不改份数——即使该草稿当前一份的应投已达上限，无变化调整也必须成功；
// 创建时传入会溢出的份数得到的是输入类错误而不是 ErrCorruptData。
func TestPlanValidationDoesNotRejectLegalCalls(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RegisterRecipe("r9", "R9", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: limitGrams},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R9", "v1", 1); err != nil {
		t.Fatalf("一份（应投恰好上限）应可创建: %v", err)
	}

	// 份数传 0 且配方留空：无变化调整，必须成功，份数保持 1。
	v, err := s.UpdateDraftBatch("u-nop", "B1", "", "", 0)
	if err != nil {
		t.Fatalf("份数传 0 表示不改份数，不应被已保存数据检查拒绝: %v", err)
	}
	if v.PlannedPortions != 1 || v.RecipeVersion != "v1" {
		t.Fatalf("无变化调整结果不正确: %+v", v)
	}

	// 创建时份数过大导致应投溢出：返回失败，但不能是 ErrCorruptData
	//（这是调用方本次的非法输入，台账里并未保存损坏数据）。
	if _, err := s.CreateBatch("b2", "B2", "R9", "v1", 2); err == nil {
		t.Fatal("两份应投超限应创建失败")
	} else if errors.Is(err, ErrCorruptData) {
		t.Fatalf("创建时的非法输入不应报 ErrCorruptData，得到 %v", err)
	}
	// 失败不留下批次。
	if _, err := s.GetBatch("B2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被拒绝的创建不应留下批次，得到 %v", err)
	}
}
