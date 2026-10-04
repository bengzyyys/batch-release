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

// 构造两条批次编号完全相同的批次记录；
// identical 为 true 时两条内容（配方版本、计划份数、状态）完全一致。
func batchDupRecords(identical bool) []*batchRecord {
	first := &batchRecord{
		BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
		PlannedPortions: 2, Status: StatusExecuting,
	}
	var second *batchRecord
	if identical {
		second = &batchRecord{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusExecuting,
		}
	} else {
		second = &batchRecord{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v2",
			PlannedPortions: 5, Status: StatusDraft,
		}
	}
	return []*batchRecord{first, second}
}

// Open 时只要存在两条批次编号完全相同的批次记录就必须返回 ErrCorruptData：
// 绑定不同配方版本、份数与状态不同、内容完全一致，三种情形都拒绝；
// 错误信息需包含重复的批次编号；不返回可用的台账对象，也不改写原文件；
// 不得挑第一条或最后一条、合并投料、自动改号或删除一条后放行。
func TestOpenRejectsDuplicateBatch(t *testing.T) {
	cases := []struct {
		name      string
		identical bool
	}{
		{"绑定不同配方版本份数状态不同", false},
		{"两条记录内容完全一致", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{
					recipeR1v1(),
					{RecipeNo: "R1", Version: "v2", Name: "配方一 v2",
						Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 200000}}},
				},
				Batches: batchDupRecords(tc.identical),
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
			if !strings.Contains(err.Error(), "B1") {
				t.Fatalf("错误信息应包含重复的批次编号 %q，得到 %v", "B1", err)
			}
			// 原台账内容保留：不得挑一条、合并投料、改号或删除一条后另存。
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

// 台账里其他批次完整、重复批次各自的投料数量也与配方吻合，同样不能放行：
// 只要一个批次编号对应两条记录，Open 整体失败。
func TestOpenDuplicateBatchAmongCompleteRecords(t *testing.T) {
	dir := t.TempDir()
	dups := batchDupRecords(false)
	// 让两条重复记录各自的投料都与绑定配方吻合，排除“数量不符”这条拒绝理由。
	dups[0].Feedings = []feedingRecord{
		{Seq: 1, MaterialNo: "M1", GramsMilli: 200000, Registrar: "张三"},
	}
	dups[1].Feedings = []feedingRecord{
		{Seq: 1, MaterialNo: "M1", GramsMilli: 1000000, Registrar: "李四"},
	}
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			recipeR1v1(),
			{RecipeNo: "R1", Version: "v2", Name: "配方一 v2",
				Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 200000}}},
		},
		Batches: append([]*batchRecord{
			{BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed},
		}, dups...),
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("其他批次完整、投料吻合也应整体拒绝，得到 %v", err)
	}
}

// 台账打开后本地文件被改坏（多出一条相同批次编号的记录）：
// 下一次查询或写入都必须返回 ErrCorruptData，即使查询的是另一个正常批次；
// 不得凭此前读取过的内容返回旧结果，也不能用保存的请求结果绕过本次损坏；
// 被拒绝的写入不留业务记录、不占用请求编号；恢复后原记录继续可用。
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
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "50", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	// 另一个完全正常的批次，用于验证“只想查看正常批次”同样被拒绝。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 1); err != nil {
		t.Fatal(err)
	}

	// 保存完好时的文件内容，随后追加一条编号同样为 B1 的批次记录。
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
		PlannedPortions: 9, Status: StatusDraft,
	})
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：重复编号、另一个正常批次，全部失败；
	// 不能凭此前读取过的内容返回旧结果。
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询重复批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使查询另一个正常批次也必须拒绝，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使只查询配方也必须拒绝，得到 %v", err)
	}

	// 写入：投料、关闭、调整、新建批次、登记配方都必须被拒绝。
	if _, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("upd-x", "B2", "", "", 9); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CreateBatch("b3", "B3", "R1", "v1", 1); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上创建批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方也应失败，得到 %v", err)
	}

	// 即使提交的是之前成功过的相同请求，也不能用保存的请求结果
	// 绕过这次台账损坏。
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "50", fixedTime, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放原成功请求也应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不留痕迹：文件内容不变，请求编号未被占用，
	// 任何批次的状态、计划与投料都没有变化。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	for _, kw := range []string{"feed-rejected", "close-b1", "upd-x", "B3", "R2"} {
		if bytes.Contains(after, []byte(kw)) {
			t.Fatalf("被拒绝的写入不应留下请求结果或业务记录（%q）", kw)
		}
	}

	// 删除重复记录恢复原状后，原有记录完整可用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if b1.Status != StatusExecuting || b1.PlannedPortions != 2 ||
		len(b1.Materials) != 1 || b1.Materials[0].RequiredGrams != "200" ||
		b1.Materials[0].ActualGrams != "50" {
		t.Fatalf("恢复后批次内容应保持原样: %+v", b1)
	}
	// 损坏前已成功的请求仍可幂等重放。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "50", fixedTime, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	// 被拒绝过的请求编号仍可用于合法提交。
	f, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 不同编号的批次绑定同一配方版本是合法组合，正常创建、查询，
// 数量核对按各自批次的计划份数计算，不能被误判为重复。
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
	for _, want := range []struct {
		batchNo  string
		required string
	}{
		{"B1", "200"},
		{"B2", "300"},
	} {
		b, err := s.GetBatch(want.batchNo)
		if err != nil {
			t.Fatalf("查询批次 %q 失败: %v", want.batchNo, err)
		}
		if b.Materials[0].RequiredGrams != want.required {
			t.Fatalf("批次 %q 应投量应按各自份数计算，得到 %+v", want.batchNo, b.Materials)
		}
	}

	// 重新打开仍应正常。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("合法台账重新打开应成功: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetBatch("B2"); err != nil {
		t.Fatalf("共用配方版本的不同批次应可正常查询: %v", err)
	}
}

// 通过正常创建入口再次创建已存在的批次仍按普通冲突处理
// （ErrDuplicateBatch），不能与文件损坏的 ErrCorruptData 混用；
// 完整台账上重复提交同一请求编号与相同内容，仍返回第一次成功的
// 结果，不产生第二条批次；失败不占用请求编号。
func TestCreateBatchExistingStillDuplicateBatch(t *testing.T) {
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
	first, err := s.CreateBatch("b1", "B1", "R1", "v1", 2)
	if err != nil {
		t.Fatal(err)
	}

	// 新请求编号创建已存在的批次：ErrDuplicateBatch，且失败不占号。
	_, err = s.CreateBatch("b1-again", "B1", "R1", "v1", 2)
	if !errors.Is(err, ErrDuplicateBatch) {
		t.Fatalf("重复创建应返回 ErrDuplicateBatch，得到 %v", err)
	}
	if errors.Is(err, ErrCorruptData) {
		t.Fatalf("普通创建冲突不应归为数据损坏，得到 %v", err)
	}
	if _, err := s.CreateBatch("b1-again", "B9", "R1", "v1", 1); err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}

	// 同一请求编号与相同内容重复提交：返回第一次成功的结果。
	replay, err := s.CreateBatch("b1", "B1", "R1", "v1", 2)
	if err != nil {
		t.Fatalf("幂等重放应成功: %v", err)
	}
	if replay.BatchNo != first.BatchNo || replay.PlannedPortions != first.PlannedPortions {
		t.Fatalf("重放应返回第一次成功的结果，得到 %+v", replay)
	}

	// 台账中 B1 仍只有一条记录。
	var st persistedState
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, b := range st.Batches {
		if b.BatchNo == "B1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("批次 B1 应只有一条记录，得到 %d 条", count)
	}
}
