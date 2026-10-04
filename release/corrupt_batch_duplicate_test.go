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

// 本文件是“同一批次编号只能对应一条批次记录”的回归保障：
// 创建入口拒绝重复编号属于写入侧规则，这里锁定的是读取侧——
// 打开台账及之后每次查询/写入的重载，只要发现两条批次编号完全相同的
// 记录，就必须返回 ErrCorruptData 并拒绝整份台账，不能让 findBatch
// 总是落在排在前面的一条上而使结果取决于记录排列顺序。

// 构造两条批次编号完全相同（B2）的批次记录。
// identical 为 true 时两条内容（配方版本、份数、状态、投料）完全一致；
// 否则绑定不同配方版本、计划份数不同、状态不同。
func duplicateBatchRecords(identical bool) []*batchRecord {
	first := &batchRecord{
		BatchNo: "B2", RecipeNo: "R1", RecipeVersion: "v1",
		PlannedPortions: 2, Status: StatusExecuting,
		Feedings: []feedingRecord{
			{Seq: 1, MaterialNo: "M1", GramsMilli: 100000, Time: time.Now(), Registrar: "张三"},
		},
	}
	var second *batchRecord
	if identical {
		second = &batchRecord{
			BatchNo: "B2", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 100000, Time: first.Feedings[0].Time, Registrar: "张三"},
			},
		}
	} else {
		second = &batchRecord{
			BatchNo: "B2", RecipeNo: "R1", RecipeVersion: "v2",
			PlannedPortions: 9, Status: StatusClosed,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M2", GramsMilli: 200000, Time: time.Now(), Registrar: "李四"},
			},
		}
	}
	return []*batchRecord{first, second}
}

// Open 时只要存在两条批次编号完全相同的批次记录就必须返回 ErrCorruptData：
// 绑定不同配方版本/份数/状态不同、两条内容完全一致，两种情形都拒绝；
// 错误信息需包含重复的批次编号；不返回可用的台账对象，也不改写原文件；
// 不得挑第一条或最后一条、合并投料、自动改号或删除一条后另存。
func TestOpenRejectsDuplicateBatch(t *testing.T) {
	for _, identical := range []bool{false, true} {
		name := "编号相同但版本份数状态不同"
		if identical {
			name = "两条记录内容完全一致"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			st := &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{
					{RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: []materialRecord{
						{MaterialNo: "M1", GramsMilli: 100000},
					}},
					{RecipeNo: "R1", Version: "v2", Name: "配方一改版", Materials: []materialRecord{
						{MaterialNo: "M2", GramsMilli: 200000},
					}},
				},
				Batches: duplicateBatchRecords(identical),
			}
			original := writeStateFile(t, dir, st)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("重复批次编号应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			if !strings.Contains(err.Error(), "B2") {
				t.Fatalf("错误信息应包含重复的批次编号 %q，得到 %v", "B2", err)
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

// 台账里其他批次完整、重复批次投料数量吻合，都不能绕过：只要一个批次编号
// 出现两条，Open 整体失败，即使调用方只想查看另一个正常批次。
func TestOpenDuplicateBatchAmongCompleteRecords(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
			}},
		},
		Batches: append(
			[]*batchRecord{
				{BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed},
			},
			// 两条 B2 内容完全一致且投料数量吻合，排除“因其他校验失败才拒绝”。
			duplicateBatchRecords(true)...,
		),
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在完整批次且重复记录投料吻合也应整体拒绝，得到 %v", err)
	}
}

// 不同编号的批次可以使用同一配方版本，不得被误判为重复。
func TestOpenAllowsDifferentBatchesSharingRecipeVersion(t *testing.T) {
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
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("不同批次共用同一配方版本应能重新打开: %v", err)
	}
	defer s2.Close()
	b1, err := s2.GetBatch("B1")
	if err != nil || b1.Materials[0].RequiredGrams != "200" {
		t.Fatalf("B1 数量核对不正确: %v %+v", err, b1)
	}
	b2, err := s2.GetBatch("B2")
	if err != nil || b2.Materials[0].RequiredGrams != "300" {
		t.Fatalf("B2 数量核对不正确: %v %+v", err, b2)
	}
}

// 台账正常打开后，文件被改坏（多出一条相同批次编号的记录）：下一次查询或
// 写入在重载时都必须返回 ErrCorruptData，即使查询/操作的是另一个正常批次，
// 也不能凭此前读到的内容返回旧结果——包括重放损坏前已成功的相同请求。
// 被拒绝的写入不改变任何状态、计划或投料，不留新记录、不占用请求编号；
// 恢复文件后原有记录与幂等行为完整可用。
func TestDuplicateBatchAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	// B1 随后被改坏（追加同编号记录）；B2 始终完整，用来证明“只想看
	// 另一个正常批次”也不能绕过整份台账的损坏检查。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "50", fixedTime, "张三"); err != nil {
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
	broken.Batches = append(broken.Batches, &batchRecord{
		BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
		PlannedPortions: 7, Status: StatusDraft,
	})
	badBytes := writeStateFile(t, dir, &broken)

	// 查询重复批次与正常批次都必须失败，且不返回部分视图。
	if v, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询重复批次应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的查询不应返回部分视图: %+v", v)
	}
	if v, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使查询另一个正常批次也必须拒绝，得到 %v", err)
	} else if v != nil {
		t.Fatalf("整份台账损坏时不应返回 B2 的部分视图: %+v", v)
	}

	// 写入：对重复批次、对正常批次投料/关闭都必须被拒绝。
	if _, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对 B1 投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-ok-batch", "B2", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对正常批次 B2 投料也应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭正常批次也应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CreateBatch("create-rejected", "B9", "R1", "v1", 1); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上创建批次也应返回 ErrCorruptData，得到 %v", err)
	}
	// 即使提交损坏前已成功过的相同请求，也不能用保存的请求结果绕过损坏检查。
	if replay, err := s.AddFeeding("feed-1", "B1", "M1", "50", fixedTime, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放旧成功请求也必须先报台账损坏，得到 %v %+v", err, replay)
	}

	// 被拒绝的写入不留痕迹：文件不变，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	for _, marker := range []string{"feed-rejected", "feed-ok-batch", "close-rejected", "create-rejected", "B9"} {
		if bytes.Contains(after, []byte(marker)) {
			t.Fatalf("被拒绝的写入不应留下请求结果或业务记录 %q", marker)
		}
	}

	// 恢复原文件后：原状态、计划与投料完整，B2 未被关闭，旧成功请求可
	// 幂等重放，被拒绝过的请求编号可合法使用，序号连续。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询 B1 应成功: %v", err)
	}
	if b1.PlannedPortions != 2 || b1.Status != StatusExecuting || len(b1.Feedings) != 1 {
		t.Fatalf("B1 应保持损坏前的状态、计划与投料: %+v", b1)
	}
	if b1.Materials[0].RequiredGrams != "200" || b1.Materials[0].ActualGrams != "50" {
		t.Fatalf("B1 数量核对应按损坏前记录计算: %+v", b1.Materials)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询 B2 应成功: %v", err)
	}
	if b2.Status != StatusExecuting {
		t.Fatalf("被拒绝的关闭不应改变 B2 状态，得到 %s", b2.Status)
	}
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "50", fixedTime, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	f, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 正常写入侧规则保持不变：用新请求编号再次创建已存在的批次返回
// ErrDuplicateBatch（不是 ErrCorruptData）；在完整台账上用同一请求编号
// 重复提交相同内容，返回第一次成功的结果，不产生第二条批次。
func TestCreateBatchDuplicateAndReplayUnchanged(t *testing.T) {
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1")

	first, err := s.CreateBatch("b1", "B1", "R1", "v1", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 新请求编号 + 已存在批次编号 → ErrDuplicateBatch。
	if _, err := s.CreateBatch("b2", "B1", "R1", "v1", 10); !errors.Is(err, ErrDuplicateBatch) {
		t.Fatalf("重复批次编号应返回 ErrDuplicateBatch，得到 %v", err)
	}
	if errors.Is(err, ErrCorruptData) {
		t.Fatalf("正常创建冲突不应归为数据损坏，得到 %v", err)
	}
	// 同请求编号 + 相同内容重放 → 第一次成功的结果，不产生第二条批次。
	replay, err := s.CreateBatch("b1", "B1", "R1", "v1", 10)
	if err != nil {
		t.Fatalf("幂等重放应成功: %v", err)
	}
	if replay.BatchNo != first.BatchNo || replay.Status != first.Status ||
		replay.PlannedPortions != first.PlannedPortions {
		t.Fatalf("重放结果应与第一次一致: %+v vs %+v", replay, first)
	}
	got, err := s.GetBatch("B1")
	if err != nil || got.Status != StatusDraft || len(got.Feedings) != 0 {
		t.Fatalf("重放不应产生第二条批次或其他变化: %v %+v", err, got)
	}
	// 被拒绝的请求编号未被占用，可用于创建另一个批次。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 1); err != nil {
		t.Fatalf("失败不应占用请求编号: %v", err)
	}
}
