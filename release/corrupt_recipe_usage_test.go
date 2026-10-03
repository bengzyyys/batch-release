package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是“已保存配方版本的每份克数必须为正数”的读取侧回归保障：
// 登记配方时拒绝零与负数属于写入侧规则，这里锁定读取时的判断与登记
// 一致——打开台账及之后的每次查询/写入重载，都必须重新检查全部配方
// 版本（包括尚未被任何批次使用的版本），不允许错误用量继续参与批次
// 数量核对或新记录登记。

// 手工构造台账：R1/v1 的 M1 为每份 badMilli 千分之一克，
// B1 绑定该版本，feedingOK 时附一条正数投料。
func writeRecipeUsageState(t *testing.T, dir string, badMilli gramsMilli, feedingOK bool) []byte {
	t.Helper()
	b := &batchRecord{
		BatchNo:         "B1",
		RecipeNo:        "R1",
		RecipeVersion:   "v1",
		PlannedPortions: 2,
		Status:          StatusExecuting,
	}
	if feedingOK {
		b.Feedings = []feedingRecord{
			{Seq: 1, MaterialNo: "M1", GramsMilli: 200000, Time: time.Now(), Registrar: "张三"},
		}
	}
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: badMilli}},
		}},
		Batches: []*batchRecord{b},
	})
}

// Open 时只要任一配方版本的任一物料每份克数为零或负数，就必须返回
// ErrCorruptData：错误信息指出配方编号、版本号、物料编号以及每份克数
// 不是正数；属于已保存数据损坏，不能报成提交参数错误（ErrInvalidInput）；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsNonPositiveRecipeUsage(t *testing.T) {
	cases := []struct {
		name  string
		milli gramsMilli
	}{
		{"零", 0},
		{"负数", -500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeRecipeUsageState(t, dir, tc.milli, true)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("每份克数不是正数应返回 ErrCorruptData，得到 %v", err)
			}
			if errors.Is(err, ErrInvalidInput) {
				t.Fatalf("已保存数据损坏不应报成提交参数错误 ErrInvalidInput，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"R1", "v1", "M1", "不是正数"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			got, readErr := os.ReadFile(filepath.Join(dir, stateFileName))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("拒绝打开不应改动台账文件")
			}
		})
	}
}

// 已有投料为正数不能让错误用量合法；批次数量恰好吻合（问题物料每份为
// 零、应投为零、无投料实投也为零，差额为零）同样不能放行本次打开。
func TestOpenRejectsRecipeUsageDespitePositiveFeedingsAndExactMatch(t *testing.T) {
	dir := t.TempDir()
	// R1/v1：M1=100（合法），M2=0（非法）。B1 计划 2 份、执行中，
	// M1 实投 200 恰好等于应投 200；M2 无投料，实投 0 = 应投 0，
	// 数量核对全部吻合，但 M2 的每份克数为零仍属损坏。
	original := writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
				{MaterialNo: "M2", GramsMilli: 0},
			},
		}},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 200000, Time: time.Now(), Registrar: "张三"},
			},
		}},
	})
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("数量恰好吻合也不能放行零用量配方，得到 %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"R1", "v1", "M2", "不是正数"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应定位到问题物料 %q，得到 %v", want, err)
		}
	}
	got, readErr := os.ReadFile(filepath.Join(dir, stateFileName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 检查适用于全部配方版本：尚未被任何批次使用的版本出现零或负数用量，
// 即使其他配方完整、使用其他版本的批次投料正常，Open 仍整体失败。
// 手工把问题用量改成正数后重新打开，配方、批次、投料与请求结果完整保留。
func TestOpenRejectsNonPositiveUsageInUnusedRecipeVersion(t *testing.T) {
	dir := t.TempDir()
	fixedTime := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
			}},
			// R1/v2 从未被任何批次使用，却带有零用量物料。
			{RecipeNo: "R1", Version: "v2", Name: "配方一改版", Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 0},
			}},
			{RecipeNo: "R2", Version: "v1", Name: "配方二", Materials: []materialRecord{
				{MaterialNo: "M9", GramsMilli: 2000},
			}},
		},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 200000, Time: fixedTime, Registrar: "张三"},
			},
		}},
	})
	if s, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("未被批次使用的配方版本出现零用量也应拒绝，得到 %v", err)
	} else if !strings.Contains(err.Error(), "v2") {
		t.Fatalf("错误信息应指出问题版本 v2，得到 %v", err)
	}

	// 唯一合法的修复方向是把问题用量改为正数；不能删除问题物料或移除整个版本。
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var repaired persistedState
	if err := json.Unmarshal(data, &repaired); err != nil {
		t.Fatal(err)
	}
	for _, r := range repaired.Recipes {
		if r.RecipeNo == "R1" && r.Version == "v2" {
			r.Materials[0].GramsMilli = 250000
		}
	}
	writeStateFile(t, dir, &repaired)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("问题用量修正为正数后应能打开: %v", err)
	}
	defer s.Close()
	r2, err := s.GetRecipe("R1", "v2")
	if err != nil || len(r2.Materials) != 1 || r2.Materials[0].Grams != "250" {
		t.Fatalf("修正后的版本应完整保留: %v %+v", err, r2)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("其他批次记录应保留: %v", err)
	}
	if b1.RecipeVersion != "v1" || len(b1.Feedings) != 1 || b1.Feedings[0].Grams != "200" {
		t.Fatalf("原批次与投料不应被改动: %+v", b1)
	}
}

// 台账打开后保存内容才出现非法每份用量：下一次配方查询、批次查询或
// 写入都必须在读取时返回 ErrCorruptData。查询不能返回部分结果或沿用
// 之前的正确用量；写入不能在损坏内容上登记新记录；拒绝后原文件不变、
// 不自动修复、不占用请求编号；恢复用量后原记录与请求行为完整可用。
func TestRecipeUsageCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M1", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	// B1：执行中绑定 R1/v1；B2：草稿绑定始终完整的 R2/v1。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R2", "v1", 5); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-b1", "B1", "M1", "100", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	// 记住此前查到的正确用量，随后把保存内容中 R1/v1 的每份克数改成 0
	// （R2/v1 与 B2 仍完整）。
	if before, err := s.GetRecipe("R1", "v1"); err != nil || before.Materials[0].Grams != "100" {
		t.Fatalf("改坏前应能读到正确用量 100，得到 %v %+v", err, before)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	for _, r := range broken.Recipes {
		if r.RecipeNo == "R1" && r.Version == "v1" {
			r.Materials[0].GramsMilli = 0
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 配方查询：问题版本与完整版本都失败，且不能沿用之前读到的 100。
	if v, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("改坏后查询问题配方应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的查询不应返回部分配方视图: %+v", v)
	}
	if v, err := s.GetRecipe("R2", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询完整配方也应在整份台账重载时失败，得到 %v", err)
	} else if v != nil {
		t.Fatalf("不应返回完整配方的部分视图: %+v", v)
	}
	// 批次查询：损坏版本的批次与完整批次同样失败，不返回核对结果。
	for _, batchNo := range []string{"B1", "B2"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("改坏后查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的批次查询不应返回部分视图: %+v", v)
		}
	}

	// 写入：无论落在损坏版本还是完整版本上，都不能在损坏内容上登记。
	if _, err := s.RegisterRecipe("r3", "R3", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CreateBatch("create-rejected", "B9", "R2", "v1", 1); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上创建批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("upd-rejected", "B2", "", "", 9); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整草稿应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.StartBatch("start-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}

	// 拒绝后原台账内容保持不变：不得把零改成最小值、取绝对值、删物料或换版本。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的操作不应改动台账文件")
	}
	for _, marker := range []string{"feed-rejected", "create-rejected", "upd-rejected", "start-rejected", "R3"} {
		if bytes.Contains(after, []byte(marker)) {
			t.Fatalf("被拒绝的写入不应留下请求记录或业务数据 %q", marker)
		}
	}

	// 恢复正确用量后：配方、批次、投料完整可用，原成功请求仍可幂等重放，
	// 被拒绝过的请求编号可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	r1, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("恢复后查询配方应成功: %v", err)
	}
	if r1.Materials[0].Grams != "100" {
		t.Fatalf("应恢复为此前的正确用量 100，得到 %+v", r1)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询批次应成功: %v", err)
	}
	if len(b1.Feedings) != 1 || b1.Feedings[0].Grams != "100" {
		t.Fatalf("原有投料应保留: %+v", b1.Feedings)
	}
	replay, err := s.AddFeeding("feed-b1", "B1", "M1", "100", fixedTime, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	f, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 正常数据的数量核对不受影响：0.001 克这样的正数每份用量合法；没有投料
// 时累计实投为零、欠投差额为负，这两类零和负数都不是要拒绝的配方用量。
// 无配方、无批次台账以及关闭重开仍按原规则工作。
func TestTinyPositiveUsageAndZeroOrNegativeReconciliationKept(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("r1", "R1", "v1", "微量配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.001"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	// 不投料：应投 0.003，实投 0，差额 -0.003（欠投差额允许为负）。
	v, err := s1.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Materials[0].RequiredGrams != "0.003" ||
		v.Materials[0].ActualGrams != "0" ||
		v.Materials[0].DifferenceGrams != "-0.003" {
		t.Fatalf("零实投与负差额属正常核对结果: %+v", v.Materials[0])
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// 关闭后重新打开仍正常，核对结果保持不变。
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("含 0.001 克每份用量的合法台账应能重新打开: %v", err)
	}
	defer s2.Close()
	r, err := s2.GetRecipe("R1", "v1")
	if err != nil || r.Materials[0].Grams != "0.001" {
		t.Fatalf("0.001 克的每份用量应完整保留: %v %+v", err, r)
	}
	v2, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Materials[0].ActualGrams != "0" || v2.Materials[0].DifferenceGrams != "-0.003" {
		t.Fatalf("重新打开后零实投与负差额应保持: %+v", v2.Materials[0])
	}
}
