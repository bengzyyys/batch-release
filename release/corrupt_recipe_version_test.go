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

// 构造一份手工台账 JSON（按持久化结构序列化）。
func writeStateFile(t *testing.T, dir string, st *persistedState) []byte {
	t.Helper()
	if st.Requests == nil {
		st.Requests = map[string]*requestRecord{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("序列化台账失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatalf("写入台账文件失败: %v", err)
	}
	return data
}

func recipeR1v1() *recipeRecord {
	return &recipeRecord{RecipeNo: "R1", Version: "v1", Name: "配方一",
		Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}}}
}

// Open 时只要有一个批次找不到绑定的配方版本就必须返回 ErrCorruptData：
// 草稿、执行中、已关闭三种状态都要保留自己绑定的配方依据；
// 错误信息需包含批次编号及其所需的配方编号、版本号；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsBatchMissingRecipeVersion(t *testing.T) {
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			original := writeStateFile(t, dir, &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{recipeR1v1()},
				Batches: []*batchRecord{{
					BatchNo:         "B-bad",
					RecipeNo:        "R1",
					RecipeVersion:   "v9", // 从未登记
					PlannedPortions: 2,
					Status:          status,
				}},
			})

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("批次缺少配方版本应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"B-bad", "R1", "v9"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			// 原台账内容保留：Open 不得删除批次或把剩余记录另存为完整台账。
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

// 配方编号相同而版本不同仍算缺失：存在 R1/v2 但批次绑定 R1/v1 时，
// 不能自动改用其他版本，Open 仍必须失败。
func TestOpenSameRecipeNoDifferentVersionStillMissing(t *testing.T) {
	dir := t.TempDir()
	r2 := recipeR1v1()
	r2.Version = "v2"
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{r2},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusDraft,
		}},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("同编号的其他版本不能替代缺失版本，得到 %v", err)
	}
}

// 台账里其他批次完整也不能绕过：只要一个批次缺配方版本，Open 整体失败。
// 恢复缺失的原配方版本后重新打开，三个批次（含各自状态与份数）都继续可用。
func TestOpenOneBrokenBatchAmongCompleteOnes(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1()},
		Batches: []*batchRecord{
			{BatchNo: "B-ok-1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusDraft},
			{BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v9", PlannedPortions: 2, Status: StatusExecuting},
			{BatchNo: "B-ok-2", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 3, Status: StatusClosed},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在完整批次也应整体拒绝，得到 %v", err)
	}

	// 恢复 B-bad 所需的原配方版本 R1/v9（不是用其他版本顶替）。
	r9 := recipeR1v1()
	r9.Version = "v9"
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1(), r9},
		Batches: []*batchRecord{
			{BatchNo: "B-ok-1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusDraft},
			{BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v9", PlannedPortions: 2, Status: StatusExecuting},
			{BatchNo: "B-ok-2", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 3, Status: StatusClosed},
		},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("恢复缺失版本后重新打开应成功: %v", err)
	}
	defer s.Close()
	for _, want := range []struct {
		no       string
		version  string
		portions int
		status   BatchStatus
	}{
		{"B-ok-1", "v1", 1, StatusDraft},
		{"B-bad", "v9", 2, StatusExecuting},
		{"B-ok-2", "v1", 3, StatusClosed},
	} {
		b, err := s.GetBatch(want.no)
		if err != nil {
			t.Fatalf("恢复后查询批次 %q 失败: %v", want.no, err)
		}
		if b.RecipeVersion != want.version || b.PlannedPortions != want.portions || b.Status != want.status {
			t.Fatalf("批次 %q 原有内容应保留，得到 %+v", want.no, b)
		}
	}
}

// 已登记但暂时没有批次使用的配方版本可以保留；无批次台账正常打开。
func TestOpenKeepsRegisteredButUnusedRecipes(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("只有配方、没有批次的台账应能正常打开: %v", err)
	}
	defer s2.Close()
	r, err := s2.GetRecipe("R1", "v1")
	if err != nil || r.Name != "配方一" {
		t.Fatalf("未被批次使用的配方版本应保留，得到 %v %+v", err, r)
	}
}

// 台账打开后本地文件被改坏（缺少批次绑定的配方版本）：
// 下一次查询或写入都必须返回 ErrCorruptData，不得返回不完整视图、
// 不得继续使用此前读到的配方、不得崩溃；操作其他完整批次也不能绕过。
// 被拒绝的写入不留业务记录、不占用请求编号；恢复缺失版本后原记录继续可用。
func TestCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	// B1：草稿绑定 R1/v1；B2：执行中绑定 R2/v1（完整批次）；B3：已关闭绑定 R1/v1。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R2", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b3", "B3", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s3", "B3"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-b2", "B2", "M1", "5", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("c3", "B3"); err != nil {
		t.Fatal(err)
	}

	// 保存完好时的文件内容，随后从文件中删掉 R1/v1（B1、B3 失去配方依据，B2 仍完整）。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	kept := broken.Recipes[:0]
	for _, r := range broken.Recipes {
		if !(r.RecipeNo == "R1" && r.Version == "v1") {
			kept = append(kept, r)
		}
	}
	broken.Recipes = kept
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：损坏批次、完整批次、配方查询，全部失败；不得崩溃。
	for _, batchNo := range []string{"B1", "B2", "B3"} {
		if _, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("文件损坏后查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
	}
	if _, err := s.GetRecipe("R2", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后不能继续使用此前读到的配方，得到 %v", err)
	}

	// 写入：对损坏批次与对完整批次的操作都必须被拒绝（不得崩溃、不得放行）。
	if _, err := s.StartBatch("start-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("upd-b1", "B1", "", "", 5); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整草稿应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-b2", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("不能借操作完整批次绕过读取失败，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r3", "R3", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方也应失败，得到 %v", err)
	}

	// 被拒绝的写入不留痕迹：文件内容不变，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	if bytes.Contains(after, []byte("feed-rejected")) || bytes.Contains(after, []byte("R3")) {
		t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
	}

	// 恢复缺失的原配方版本后，原有记录完整可用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if b2.Status != StatusExecuting || len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
		t.Fatalf("原有投料记录应保留: %+v", b2)
	}
	b3, err := s.GetBatch("B3")
	if err != nil {
		t.Fatalf("恢复后查询已关闭批次应成功: %v", err)
	}
	if b3.Status != StatusClosed {
		t.Fatalf("关闭含义不变，恢复后仍应为已关闭: %+v", b3)
	}
	// 损坏前已成功的请求仍可幂等重放。
	replay, err := s.AddFeeding("feed-b2", "B2", "M1", "5", fixedTime, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	// 被拒绝过的请求编号仍可用于合法提交。
	f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}
