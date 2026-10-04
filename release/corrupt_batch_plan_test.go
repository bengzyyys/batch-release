package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// maxRecipeRecord 是一份每份恰好需要上限克数的配方记录（另有 M2 物料）。
func maxRecipeRecord(recipeNo, version string) *recipeRecord {
	return &recipeRecord{RecipeNo: recipeNo, Version: version, Name: "上限配方",
		Materials: []materialRecord{
			{MaterialNo: "Mbig", GramsMilli: gramsMilli(math.MaxInt64)},
			{MaterialNo: "M2", GramsMilli: 1000},
		}}
}

// Open 时已保存批次的计划份数为零或负数，必须返回 ErrCorruptData：
// 草稿、执行中、已关闭三种状态一视同仁；错误信息需包含批次编号与份数；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsNonPositivePlannedPortions(t *testing.T) {
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		for _, portions := range []int{0, -1, -50} {
			name := string(status) + "-份数" + strconv.Itoa(portions)
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				original := writeStateFile(t, dir, &persistedState{
					Version: stateVersion,
					Recipes: []*recipeRecord{recipeR1v1()},
					Batches: []*batchRecord{{
						BatchNo:         "B-bad",
						RecipeNo:        "R1",
						RecipeVersion:   "v1",
						PlannedPortions: portions,
						Status:          status,
					}},
				})

				s, err := Open(dir)
				if !errors.Is(err, ErrCorruptData) {
					t.Fatalf("份数 %d（状态 %s）应返回 ErrCorruptData，得到 %v", portions, status, err)
				}
				if errors.Is(err, ErrInvalidInput) {
					t.Fatalf("已保存份数非法属于数据损坏，不应报成提交参数错误，得到 %v", err)
				}
				if s != nil {
					s.Close()
					t.Fatalf("损坏台账不应返回可用的 Store 对象")
				}
				msg := err.Error()
				for _, want := range []string{"B-bad", "正整数", strconv.Itoa(portions)} {
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

// 某物料按绑定配方版本与计划份数算出的应投量超过上限时，
// Open 必须返回 ErrCorruptData：错误信息需包含批次编号、计划份数、
// 物料编号以及绑定的配方编号与版本号；不返回可用对象，不改写原文件。
func TestOpenRejectsPlannedRequirementOverflow(t *testing.T) {
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			original := writeStateFile(t, dir, &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{maxRecipeRecord("R9", "v1")},
				Batches: []*batchRecord{{
					// Mbig 每份即上限，两份必然溢出；M2 两份仍合法，
					// 但整份台账必须按损坏处理。
					BatchNo:         "B-bad",
					RecipeNo:        "R9",
					RecipeVersion:   "v1",
					PlannedPortions: 2,
					Status:          status,
				}},
			})

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("应投量超限应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"B-bad", "2", "Mbig", "R9", "v1"} {
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

// 单种物料应投量恰好等于上限合法；多种物料各自达到上限，
// 不应因合计超过上限而被拒绝。
func TestOpenAcceptsPlannedRequirementAtLimitAndPerMaterial(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R9", Version: "v1", Name: "上限配方",
			Materials: []materialRecord{
				// 两种物料每份都恰好是上限：一份时各自应投等于上限，
				// 合计是两倍上限，但上限按物料分别判断，仍属合法。
				{MaterialNo: "Mbig1", GramsMilli: gramsMilli(math.MaxInt64)},
				{MaterialNo: "Mbig2", GramsMilli: gramsMilli(math.MaxInt64)},
			},
		}},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R9", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusExecuting,
		}},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("各物料应投各自恰好等于上限应正常打开: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]MaterialRequirement{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m
	}
	for _, mat := range []string{"Mbig1", "Mbig2"} {
		if got[mat].RequiredGrams != limitGrams {
			t.Fatalf("物料 %s 应投应为上限 %s，得到 %s", mat, limitGrams, got[mat].RequiredGrams)
		}
		if got[mat].ActualGrams != "0" || got[mat].DifferenceGrams != "-"+limitGrams {
			t.Fatalf("未投料物料核对结果不正确: %+v", got[mat])
		}
	}
}

// 其他批次完整也不能绕过：一个批次份数非法，Open 整体失败，
// 查询哪个批次都不应该成为放行理由。
func TestOpenOneBatchWithBadPortionsFailsWholeLedger(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1(), maxRecipeRecord("R9", "v1")},
		Batches: []*batchRecord{
			{BatchNo: "B-ok-1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusDraft},
			{BatchNo: "B-bad", RecipeNo: "R9", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusExecuting},
			{BatchNo: "B-ok-2", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 3, Status: StatusClosed},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在完整批次也应整体拒绝，得到 %v", err)
	}
}

// 台账打开后文件中批次份数被改成超限值（或零/负数）：
// 下一次查询或写入都必须返回 ErrCorruptData，即使操作的是另一个正常批次；
// 被拒绝的写入不改动文件、不占用请求编号；恢复合法份数后原记录继续可用。
func TestBatchPlanCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*batchRecord)
		wantMsg []string
	}{
		{"份数改成两份导致应投超限", func(b *batchRecord) { b.PlannedPortions = 2 },
			[]string{"B1", "2", "Mbig", "R9", "v1"}},
		{"份数改成零", func(b *batchRecord) { b.PlannedPortions = 0 },
			[]string{"B1", "0", "正整数"}},
		{"份数改成负数", func(b *batchRecord) { b.PlannedPortions = -3 },
			[]string{"B1", "-3", "正整数"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
				{MaterialNo: "M1", Grams: "100"},
			}); err != nil {
				t.Fatal(err)
			}
			// B1：执行中，一份上限配方（合法）；B2：执行中的正常批次。
			if _, err := s.CreateBatch("b1", "B1", "R9", "v1", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartBatch("start-b1", "B1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 10); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartBatch("start-b2", "B2"); err != nil {
				t.Fatal(err)
			}
			fixedTime := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
			if _, err := s.AddFeeding("feed-b2", "B2", "M1", "5", fixedTime, "张三"); err != nil {
				t.Fatal(err)
			}

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
					tc.mutate(b)
				}
			}
			badBytes := writeStateFile(t, dir, &broken)

			// 查询损坏批次与正常批次、配方都必须失败。
			for _, batchNo := range []string{"B1", "B2"} {
				_, err := s.GetBatch(batchNo)
				if !errors.Is(err, ErrCorruptData) {
					t.Fatalf("文件损坏后查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
				}
				for _, want := range tc.wantMsg {
					if batchNo == "B1" && !strings.Contains(err.Error(), want) {
						t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
					}
				}
			}
			if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("整份台账损坏后配方查询也应失败，得到 %v", err)
			}

			// 对正常批次 B2 的写入、登记新配方都不能借操作对象绕过。
			if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.CloseBatch("close-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上关闭批次应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.RegisterRecipe("r-rejected", "R3", "v1", "新配方", []MaterialInput{
				{MaterialNo: "M1", Grams: "1"},
			}); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
			}

			// 被拒绝的操作不改文件、不留请求记录。
			after, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, badBytes) {
				t.Fatalf("被拒绝的写入不应改动台账文件")
			}
			if bytes.Contains(after, []byte("feed-rejected")) ||
				bytes.Contains(after, []byte("close-rejected")) ||
				bytes.Contains(after, []byte("R3")) {
				t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
			}

			// 恢复合法计划后原记录完整可用；被拒绝过的编号仍可合法提交。
			if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
				t.Fatal(err)
			}
			b1, err := s.GetBatch("B1")
			if err != nil {
				t.Fatalf("恢复后查询 B1 应成功: %v", err)
			}
			if b1.PlannedPortions != 1 || b1.Status != StatusExecuting {
				t.Fatalf("B1 原计划应保留: %+v", b1)
			}
			if b1.Materials[0].RequiredGrams != limitGrams {
				t.Fatalf("B1 应投量应为上限，得到 %s", b1.Materials[0].RequiredGrams)
			}
			b2, err := s.GetBatch("B2")
			if err != nil {
				t.Fatalf("恢复后查询 B2 应成功: %v", err)
			}
			if len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
				t.Fatalf("B2 原有投料应保留: %+v", b2.Feedings)
			}
			if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); err != nil {
				t.Fatalf("被拒绝过的请求编号应仍可合法使用: %v", err)
			}
			if c, err := s.CloseBatch("close-rejected", "B2"); err != nil || c.Status != StatusClosed {
				t.Fatalf("被拒绝过的关闭编号应仍可合法使用: %v %+v", err, c)
			}
		})
	}
}

// 提交时若份数会让应投量超限，属于本次输入不合法（ErrInvalidInput），
// 而不是先落盘再等读取报损坏：不保存批次、不占用请求编号。
func TestCreateBatchPlannedRequirementOverflowIsInvalidInput(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("r9", "R9", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: limitGrams},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("bad", "B1", "R9", "v1", 2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("创建时应投超限应返回 ErrInvalidInput，得到 %v", err)
	}
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被拒绝的创建不应留下批次，得到 %v", err)
	}
	// 同一请求编号修正为一份（恰好上限）后应成功。
	b, err := s.CreateBatch("bad", "B1", "R9", "v1", 1)
	if err != nil {
		t.Fatalf("失败不应占用请求编号，合法份数应成功: %v", err)
	}
	mats := materialsMap(b)
	checkRequirement(t, mats, "Mbig", limitGrams, "0", "-"+limitGrams)
	checkRequirement(t, mats, "M2", "1", "0", "-1")
}

// 调整草稿时最终计划应投超限必须整体拒绝（ErrInvalidInput），不留下半调整；
// 份数传 0 表示不改份数的语义不变——即使因此最终计划会超限，拒绝的也只是
// 本次提交，不能把“0 = 不改份数”当成非法份数。失败不占用请求编号。
func TestUpdateDraftPlannedRequirementOverflowRejected(t *testing.T) {
	s := openTestStore(t)
	registerTwoRecipeVersions(t, s) // R1/v1: M1=100,M2=0.5,M3=0.010；R1/v2: M1=250,M4=2
	if _, err := s.RegisterRecipe("r9", "R9", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: limitGrams},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}

	// 只改份数为 2：沿用 R1/v1，100×2 合法，应成功（对照组）。
	if _, err := s.UpdateDraftBatch("u-ok", "B1", "", "", 2); err != nil {
		t.Fatalf("合法份数调整应成功: %v", err)
	}
	// 改选上限配方但份数传 0：语义是“份数保持 2 不变”，Mbig×2 超限，
	// 整体拒绝，原配方与份数保留。
	_, err := s.UpdateDraftBatch("u-over-keep", "B1", "R9", "v1", 0)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("份数传 0 仍表示不改份数，最终计划超限应返回 ErrInvalidInput，得到 %v", err)
	}
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeNo != "R1" || got.RecipeVersion != "v1" || got.PlannedPortions != 2 {
		t.Fatalf("失败后应保留原配方与份数，得到 %+v", got)
	}
	// 同一编号改成一份（恰好上限）应成功，证明份数 0 没有被当成“非法份数”
	// 永久占用编号，失败也没占用编号。
	v, err := s.UpdateDraftBatch("u-over-keep", "B1", "R9", "v1", 1)
	if err != nil {
		t.Fatalf("失败不应占用请求编号，合法调整应成功: %v", err)
	}
	if v.RecipeVersion != "v1" || v.PlannedPortions != 1 {
		t.Fatalf("成功结果应指向 R9/v1 一份，得到 %+v", v)
	}
	checkRequirement(t, materialsMap(v), "Mbig", limitGrams, "0", "-"+limitGrams)

	// 同时改配方与份数为两份：超限，整体回滚到调整前的合法计划。
	if _, err := s.UpdateDraftBatch("u-back-r1", "B1", "R1", "v1", 2); err != nil {
		t.Fatalf("前置切回基础配方应成功: %v", err)
	}
	_, err = s.UpdateDraftBatch("u-over-combo", "B1", "R9", "v1", 2)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("组合调整导致超限应返回 ErrInvalidInput，得到 %v", err)
	}
	got, _ = s.GetBatch("B1")
	if got.RecipeNo != "R1" || got.RecipeVersion != "v1" || got.PlannedPortions != 2 {
		t.Fatalf("组合调整失败应整体回滚，得到 %+v", got)
	}

	// 份数传 0 的无变化调整依旧合法（明确不能把已保存数据检查变成
	// 对合法调用的拒绝）；改选版本但保持当前份数不变也同理。
	if _, err := s.UpdateDraftBatch("u-nop", "B1", "", "", 0); err != nil {
		t.Fatalf("份数传 0 的无变化调整应继续合法: %v", err)
	}
	if _, err := s.UpdateDraftBatch("u-keep-two", "B1", "R9", "v1", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("保持 2 份切到上限配方仍应因最终计划超限返回 ErrInvalidInput，得到 %v", err)
	}
	got, _ = s.GetBatch("B1")
	if got.RecipeNo != "R1" || got.PlannedPortions != 2 {
		t.Fatalf("两次拒绝都不应改动批次，得到 %+v", got)
	}
}

// 计划合法但尚未投料、实投不足或超出应投都不影响读取；
// 执行中批次仍可按原规则关闭，关闭后重开仍可查询，不新增数量吻合要求。
func TestLegalPlanWithAnyFeedingStateStillUsable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r9", "R9", "v1", "上限配方", []MaterialInput{
		{MaterialNo: "Mbig", Grams: limitGrams},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	// 一份上限配方：未投料的草稿即可查询。
	if _, err := s.CreateBatch("bd", "B-draft", "R9", "v1", 1); err != nil {
		t.Fatal(err)
	}
	dv, err := s.GetBatch("B-draft")
	if err != nil {
		t.Fatal(err)
	}
	dm := materialsMap(dv)
	checkRequirement(t, dm, "Mbig", limitGrams, "0", "-"+limitGrams)

	if _, err := s.CreateBatch("b1", "B1", "R9", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// 实投远低于应投（欠投），随后关闭——不要求数量吻合。
	if _, err := s.AddFeeding("f1", "B1", "M2", "1", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	under, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	um := materialsMap(under)
	checkRequirement(t, um, "Mbig", limitGrams, "0", "-"+limitGrams)
	checkRequirement(t, um, "M2", "1", "1", "0")
	c, err := s.CloseBatch("c1", "B1")
	if err != nil {
		t.Fatalf("欠投也应允许按原规则关闭: %v", err)
	}
	if c.Status != StatusClosed {
		t.Fatalf("关闭后状态应为已关闭，得到 %s", c.Status)
	}

	// 超投场景：另一执行中批次，M2 应投 1 克，投 2 克，仍可查询与关闭。
	if _, err := s.CreateBatch("b2", "B2", "R9", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B2", "M2", "2", time.Now(), "李四"); err != nil {
		t.Fatal(err)
	}
	over, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	om := materialsMap(over)
	checkRequirement(t, om, "M2", "1", "2", "1")
	if _, err := s.CloseBatch("c2", "B2"); err != nil {
		t.Fatalf("超投也应允许关闭: %v", err)
	}

	// 关闭后重新打开：全部批次仍可查询，状态与数量核对不变。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("含上限应投、欠投/超投/未投料批次的台账应能重新打开: %v", err)
	}
	defer s2.Close()
	for _, want := range []struct {
		no     string
		status BatchStatus
	}{
		{"B-draft", StatusDraft}, {"B1", StatusClosed}, {"B2", StatusClosed},
	} {
		b, err := s2.GetBatch(want.no)
		if err != nil {
			t.Fatalf("重开后查询批次 %q 失败: %v", want.no, err)
		}
		if b.Status != want.status {
			t.Fatalf("批次 %q 状态应保留为 %s，得到 %s", want.no, want.status, b.Status)
		}
	}
}
