package release

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ledgerPath 是测试用台账文件路径（与常量 stateFileName 一致）。
func ledgerPath(dir string) string { return filepath.Join(dir, stateFileName) }

// editLedger 在 Store 之外直接改写台账文件，模拟本地台账内容在程序外发生变化。
func editLedger(t *testing.T, dir string, fn func(st *persistedState)) {
	t.Helper()
	data, err := os.ReadFile(ledgerPath(dir))
	if err != nil {
		t.Fatalf("读取台账文件失败: %v", err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("解析台账文件失败: %v", err)
	}
	fn(&st)
	out, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatalf("序列化台账文件失败: %v", err)
	}
	if err := os.WriteFile(ledgerPath(dir), out, 0o644); err != nil {
		t.Fatalf("写回台账文件失败: %v", err)
	}
}

func removeRecipeRecord(st *persistedState, recipeNo, version string) {
	kept := st.Recipes[:0]
	for _, r := range st.Recipes {
		if r.RecipeNo == recipeNo && r.Version == version {
			continue
		}
		kept = append(kept, r)
	}
	st.Recipes = kept
}

func readLedgerState(t *testing.T, dir string) *persistedState {
	t.Helper()
	data, err := os.ReadFile(ledgerPath(dir))
	if err != nil {
		t.Fatalf("读取台账文件失败: %v", err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("解析台账文件失败: %v", err)
	}
	return &st
}

func checkCorruptError(t *testing.T, err error, batchNo, recipeNo, version string) {
	t.Helper()
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("应返回 ErrCorruptData，得到 %v", err)
	}
	msg := err.Error()
	for _, want := range []string{batchNo, recipeNo, version} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含问题批次编号 %q 及其所需配方编号、版本号，得到 %q", want, msg)
		}
	}
}

// buildLedgerWithBatch 建立包含一个指定状态批次的台账后关闭，返回目录。
func buildLedgerWithBatch(t *testing.T, status BatchStatus) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	switch status {
	case StatusExecuting:
		if _, err := s.StartBatch("s1", "B1"); err != nil {
			t.Fatal(err)
		}
	case StatusClosed:
		if _, err := s.StartBatch("s1", "B1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddFeeding("f0", "B1", "M1", "10", time.Now(), "张三"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseBatch("c1", "B1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// 读取已有台账时，只要有一个批次找不到共同匹配的已登记配方版本，
// Open 就必须返回 ErrCorruptData 且不返回可继续使用的台账对象。
// 草稿、执行中、已关闭三种状态的批次都需要保留自己绑定的配方依据。
func TestOpenRejectsBatchMissingRecipeVersion(t *testing.T) {
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			dir := buildLedgerWithBatch(t, status)
			editLedger(t, dir, func(st *persistedState) {
				removeRecipeRecord(st, "R1", "v1")
			})

			s, err := Open(dir)
			checkCorruptError(t, err, "B1", "R1", "v1")
			if s != nil {
				s.Close()
				t.Fatalf("数据损坏时不应返回可继续使用的台账对象")
			}

			// 原台账内容保留：问题批次未被删除，也没有补造配方。
			st := readLedgerState(t, dir)
			if len(st.Batches) != 1 || st.Batches[0].BatchNo != "B1" {
				t.Fatalf("拒绝读取不应删除问题批次，得到 %+v", st.Batches)
			}
			if len(st.Recipes) != 0 {
				t.Fatalf("拒绝读取不应补造配方，得到 %+v", st.Recipes)
			}
		})
	}
}

// 配方编号相同而版本不同仍算缺失：存在 R1/v2 不能替代批次绑定的 R1/v1，
// 也不能根据名称或物料内容替换。
func TestOpenSameRecipeNoDifferentVersionStillMissing(t *testing.T) {
	dir := buildLedgerWithBatch(t, StatusDraft)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r2", "R1", "v2", "改版配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "200"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	editLedger(t, dir, func(st *persistedState) {
		removeRecipeRecord(st, "R1", "v1") // 只保留同编号的 R1/v2
	})

	_, err = Open(dir)
	checkCorruptError(t, err, "B1", "R1", "v1")
}

// 即使台账中还有其他完整批次，缺少配方版本的批次也会让整份台账无法打开，
// 不能只打开剩余完整批次或通过操作它们绕过失败。
func TestOpenOneBadBatchRejectsEntireLedger(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M2", Grams: "5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R2", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	editLedger(t, dir, func(st *persistedState) {
		removeRecipeRecord(st, "R2", "v1") // B2 失去配方依据，B1 仍完整
	})

	s2, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在一个坏批次时整份台账都不能打开，得到 %v", err)
	}
	if s2 != nil {
		s2.Close()
		t.Fatalf("不应返回可操作完整批次 B1 的台账对象")
	}
}

// 打开时数据完整，随后台账文件在程序外被改成缺少批次绑定的配方版本：
// 下一次查询或写入必须返回 ErrCorruptData，不能返回不完整视图、
// 不能沿用此前读到的配方，也不能崩溃（旧实现开始执行/关闭批次会空指针崩溃）。
func TestExternallyCorruptedLedgerFailsEveryOperation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "标准配方", []MaterialInput{
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
	if _, err := s.AddFeeding("f1", "B1", "M1", "10", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	// 另一完整批次，确认操作它也无法绕过这次读取失败。
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R2", "v1", 3); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(ledgerPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	editLedger(t, dir, func(st *persistedState) {
		removeRecipeRecord(st, "R1", "v1")
	})
	// 损坏落盘后的文件作为基准：之后被拒绝的写入不应再改动它。
	corruptBytes, err := os.ReadFile(ledgerPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(corruptBytes) == string(before) {
		t.Fatalf("前置条件：台账文件应已被外部改写为损坏状态")
	}

	// 查询坏批次、完整批次、配方都必须失败。
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("坏批次查询应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("不能通过查询其他完整批次绕过读取失败，得到 %v", err)
	}
	if _, err := s.GetRecipe("R2", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("台账损坏期间不能继续使用此前读到的配方，得到 %v", err)
	}
	// 旧实现对缺失配方的批次调用开始执行/关闭会直接崩溃：这里必须返回错误。
	if _, err := s.CloseBatch("c2", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("坏批次关闭应返回 ErrCorruptData（旧实现会崩溃），得到 %v", err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M1", "1", time.Now(), "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("坏批次投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.StartBatch("b3new", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("台账损坏期间其他写入也必须失败，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r3", "R3", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M3", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("台账损坏期间不能写入新配方，得到 %v", err)
	}
	// 已成功请求的幂等重放也不能绕过这次读取失败。
	if _, err := s.AddFeeding("f1", "B1", "M1", "10", time.Now(), "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放旧请求也应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不改变、不另存台账内容：文件与损坏落盘后逐字节一致，
	// 没有新业务记录或请求结果落盘，也没有把剩余记录当作完整台账保存。
	after, err := os.ReadFile(ledgerPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(corruptBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
}

// 草稿批次失去配方版本后，调整草稿与开始执行都必须返回 ErrCorruptData 而非崩溃。
func TestExternallyCorruptedDraftFailsWithoutCrash(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	editLedger(t, dir, func(st *persistedState) {
		removeRecipeRecord(st, "R1", "v1")
	})

	if _, err := s.StartBatch("s2", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("坏草稿开始执行应返回 ErrCorruptData（旧实现会崩溃），得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("u2", "B1", "", "", 20); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("坏草稿调整应返回 ErrCorruptData，得到 %v", err)
	}
}

// 被拒绝的写入不留下新业务记录或请求结果；恢复缺失的原配方版本后，
// 此前被拒绝的请求编号仍可用于合法提交，原有记录完整可用。
func TestRejectedRequestNoUsableAfterRecipeRestored(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "标准配方", []MaterialInput{
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

	editLedger(t, dir, func(st *persistedState) {
		removeRecipeRecord(st, "R1", "v1")
	})

	// 投料请求因台账损坏被拒绝。
	if _, err := s.AddFeeding("f-later", "B1", "M1", "5", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏期间投料应返回 ErrCorruptData，得到 %v", err)
	}
	// 关闭请求同样被拒绝，且其编号不应被占用。
	if _, err := s.CloseBatch("c-later", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏期间关闭应返回 ErrCorruptData，得到 %v", err)
	}

	// 恢复缺失的原配方版本（编号、版本号与原绑定完全一致），不重新打开 Store，
	// 再次发起操作即可正常使用原有记录。
	editLedger(t, dir, func(st *persistedState) {
		st.Recipes = append(st.Recipes, &recipeRecord{
			RecipeNo: "R1",
			Version:  "v1",
			Name:     "标准配方",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
			},
		})
	})

	// 此前被拒绝的投料编号仍可合法提交，序号从 1 开始（之前没有落盘）。
	feeding, err := s.AddFeeding("f-later", "B1", "M1", "5", time.Now(), "李四")
	if err != nil {
		t.Fatalf("恢复配方版本后，被拒绝的请求编号应可正常提交: %v", err)
	}
	if feeding.Seq != 1 || feeding.Grams != "5" {
		t.Fatalf("恢复后投料应为全新成功记录（seq=1），得到 %+v", feeding)
	}
	// 此前被拒绝的关闭编号也可正常提交，批次确实被关闭。
	closed, err := s.CloseBatch("c-later", "B1")
	if err != nil {
		t.Fatalf("恢复配方版本后关闭应成功: %v", err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("批次应已关闭，得到 %s", closed.Status)
	}

	// 原有批次的状态、投料明细与数量核对含义不变；关闭仍只是确认投料，
	// 不要求数量吻合（应投 1000，实投 5，差额 -995 照样关闭）。
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if view.PlannedPortions != 10 || len(view.Feedings) != 1 {
		t.Fatalf("原有批次记录应完整保留，得到 %+v", view)
	}
	if view.Materials[0].RequiredGrams != "1000" ||
		view.Materials[0].ActualGrams != "5" ||
		view.Materials[0].DifferenceGrams != "-995" {
		t.Fatalf("数量核对应保持原含义，得到 %+v", view.Materials[0])
	}
}

// 恢复缺失配方后重新 Open 也能正常使用原有记录。
func TestReopenWorksAfterRecipeRestored(t *testing.T) {
	dir := buildLedgerWithBatch(t, StatusClosed)
	editLedger(t, dir, func(st *persistedState) {
		removeRecipeRecord(st, "R1", "v1")
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("前置条件：缺失配方版本时应无法打开，得到 %v", err)
	}

	editLedger(t, dir, func(st *persistedState) {
		st.Recipes = append(st.Recipes, &recipeRecord{
			RecipeNo: "R1",
			Version:  "v1",
			Name:     "标准配方",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
			},
		})
	})

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("恢复原配方版本后应能重新打开: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusClosed || view.RecipeVersion != "v1" || len(view.Feedings) != 1 {
		t.Fatalf("原有记录应完整可用，得到 %+v", view)
	}
}

// 首次打开尚无台账文件的目录仍得到空台账。
func TestOpenMissingFileStillEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("无台账文件的目录应得到空台账: %v", err)
	}
	defer s.Close()
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空台账查询批次应返回 ErrNotFound，得到 %v", err)
	}
}

// 已登记但暂时没有批次使用的配方版本可以保留：只有配方、没有批次的台账正常打开。
func TestOpenKeepsRecipeWithoutBatches(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("r1", "R1", "v1", "暂无批次的配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("只有配方没有批次的台账应正常打开: %v", err)
	}
	defer s2.Close()
	r, err := s2.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("未被批次使用的配方版本应保留可查: %v", err)
	}
	if r.Name != "暂无批次的配方" {
		t.Fatalf("配方内容不正确: %+v", r)
	}
}
