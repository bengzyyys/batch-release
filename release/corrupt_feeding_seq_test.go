package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件锁定“同一批次的投料登记序号必须与登记位置一致”的读取侧约束：
// 打开台账以及打开后的每次查询/写入重载，都必须重新执行这一检查。
// 序号是整个批次内全部投料共用的连续序列（不分物料），从 1 开始；
// 不同批次各自从 1 开始。

// 构造含两种物料（M1、M2）的 R1/v1 配方，以及一个带指定投料的批次。
// 投料的物料归属与数量都合法，使序号成为唯一的拒绝理由。
func writeSeqState(t *testing.T, dir string, status BatchStatus, feedings []feedingRecord) []byte {
	t.Helper()
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
				{MaterialNo: "M2", GramsMilli: 50000},
			},
		}},
		Batches: []*batchRecord{{
			BatchNo:         "B-bad",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: 2,
			Status:          status,
			Feedings:        feedings,
		}},
	})
}

func seqFeedings(seqs ...int) []feedingRecord {
	now := time.Now()
	out := make([]feedingRecord, 0, len(seqs))
	for i, seq := range seqs {
		// 物料在 M1、M2 间交替：序号是全批次共用序列，不按物料另起编号。
		mat := "M1"
		if i%2 == 1 {
			mat = "M2"
		}
		out = append(out, feedingRecord{
			Seq: seq, MaterialNo: mat, GramsMilli: 1000, Time: now, Registrar: "张三",
		})
	}
	return out
}

// Open 读取时，投料序号不为“从 1 开始、逐条加 1、与位置一致”必须返回
// ErrCorruptData：零、负数、漏号、重复、跳号，以及只调换记录位置而未同步
// 序号都算损坏；即使物料归属与数量全部合法也不能放行。错误信息必须指出
// 问题批次编号、该记录在列表中的位置、实际序号与应有序号；不返回可用的
// Store，也不改写原文件。
func TestOpenRejectsFeedingSeqNotMatchingPosition(t *testing.T) {
	cases := []struct {
		name       string
		seqs       []int
		pos        int // 第一个失去顺序的记录位置（从 1 开始）
		wantActual int // 该位置实际保存的序号
		wantExpect int // 该位置应有的序号
	}{
		{"首条序号为零", []int{0}, 1, 0, 1},
		{"首条序号为负数", []int{-1}, 1, -1, 1},
		{"未从1开始", []int{2, 3}, 1, 2, 1},
		{"重复序号1、1、3", []int{1, 1, 3}, 2, 1, 2},
		{"跳号1、3", []int{1, 3}, 2, 3, 2},
		{"调换位置未同步序号1、3、2", []int{1, 3, 2}, 2, 3, 2},
		{"中间漏号1、2、4", []int{1, 2, 4}, 3, 4, 3},
		{"中间出现零1、0、3", []int{1, 0, 3}, 2, 0, 2},
	}
	// 执行中、已关闭批次的历史投料同样受序号规则约束，不能跳过坏记录。
	for _, status := range []BatchStatus{StatusExecuting, StatusClosed} {
		for _, tc := range cases {
			t.Run(string(status)+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				original := writeSeqState(t, dir, status, seqFeedings(tc.seqs...))

				s, err := Open(dir)
				if !errors.Is(err, ErrCorruptData) {
					if s != nil {
						s.Close()
					}
					t.Fatalf("序号失去顺序应返回 ErrCorruptData，得到 %v", err)
				}
				if s != nil {
					s.Close()
					t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
				}
				msg := err.Error()
				for _, want := range []string{
					"B-bad",
					"第 " + strconv.Itoa(tc.pos) + " 条",
					strconv.Itoa(tc.wantActual),
					strconv.Itoa(tc.wantExpect),
				} {
					if !strings.Contains(msg, want) {
						t.Fatalf("错误信息应包含 %q（批次/位置/实际序号/应有序号），得到 %v", want, err)
					}
				}
				// 唯一理由是序号失序，不能与归属、数量原因混淆。
				for _, other := range []string{"不属于", "不是正数", "超出上限", "草稿"} {
					if strings.Contains(msg, other) {
						t.Fatalf("错误信息不应包含 %q，得到 %v", other, err)
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
}

// 空投料列表不构成序号错误：草稿、执行中、已关闭的无投料批次仍按原状态
// 规则处理；多物料共用同一序列，以及不同批次各自从 1 开始也在此锁定。
func TestEmptyFeedingsAndPerBatchSequencesAccepted(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
				{MaterialNo: "M2", GramsMilli: 50000},
			},
		}},
		Batches: []*batchRecord{
			{BatchNo: "B-empty-draft", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusDraft},
			{BatchNo: "B-empty-exec", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusExecuting},
			{BatchNo: "B-empty-closed", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusClosed},
			// 不同物料共用所在批次的 1、2、3 序列，不按物料另起编号。
			{BatchNo: "B-multi", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusExecuting,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: now, Registrar: "张三"},
					{Seq: 2, MaterialNo: "M2", GramsMilli: 1000, Time: now, Registrar: "张三"},
					{Seq: 3, MaterialNo: "M1", GramsMilli: 1000, Time: now, Registrar: "张三"},
				}},
			// 另一个批次的序号重新从 1 开始，与 B-multi 的序列互不影响。
			{BatchNo: "B-own-seq", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusExecuting,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: now, Registrar: "李四"},
				}},
		},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("空投料列表与各自独立序号的台账应正常打开: %v", err)
	}
	defer s.Close()
	for _, batchNo := range []string{"B-empty-draft", "B-empty-exec", "B-empty-closed"} {
		v, err := s.GetBatch(batchNo)
		if err != nil {
			t.Fatalf("查询 %q 失败: %v", batchNo, err)
		}
		if len(v.Feedings) != 0 {
			t.Fatalf("%q 应无投料: %+v", batchNo, v.Feedings)
		}
	}
	multi, err := s.GetBatch("B-multi")
	if err != nil {
		t.Fatal(err)
	}
	if len(multi.Feedings) != 3 {
		t.Fatalf("B-multi 应有 3 条投料，得到 %d", len(multi.Feedings))
	}
	for i, wantSeq := range []int{1, 2, 3} {
		if multi.Feedings[i].Seq != wantSeq {
			t.Fatalf("B-multi 第 %d 条序号应为 %d: %+v", i+1, wantSeq, multi.Feedings[i])
		}
	}
	own, err := s.GetBatch("B-own-seq")
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Feedings) != 1 || own.Feedings[0].Seq != 1 {
		t.Fatalf("B-own-seq 应独立从序号 1 开始: %+v", own.Feedings)
	}
}

// 投料时间不参与序号判断：多条投料时间相同，或后一条填写的时间早于前一条，
// 只要序号与登记位置一致，读取与查询都正常并保留登记顺序。
func TestFeedingTimesDoNotAffectSeqValidation(t *testing.T) {
	dir := t.TempDir()
	t1 := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}},
		}},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusExecuting,
			Feedings: []feedingRecord{
				// 时间相同；第 3 条时间早于第 2 条，但登记顺序仍为 1、2、3。
				{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: t1, Registrar: "张三"},
				{Seq: 2, MaterialNo: "M1", GramsMilli: 1000, Time: t1, Registrar: "李四"},
				{Seq: 3, MaterialNo: "M1", GramsMilli: 1000, Time: t1.Add(-time.Hour), Registrar: "王五"},
			},
		}},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("时间相同或逆序不应判为损坏: %v", err)
	}
	defer s.Close()
	v, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range v.Feedings {
		if f.Seq != i+1 {
			t.Fatalf("登记顺序应保留为 1、2、3，得到 %+v", v.Feedings)
		}
	}
}

// 台账正常打开后，保存内容的序号被改坏：下一次查询或写入重载时必须返回
// ErrCorruptData，不能沿用此前正常的内容；查询损坏批次、正常批次、配方都
// 不能绕过；对正常批次追加投料、关闭批次，以及对损坏的执行中批次继续追加
// 投料，同样不能绕过。被拒绝的写入不保存业务变更、不占用请求编号，原台账
// 内容不变；恢复后原有记录与请求行为完整可用。
func TestFeedingSeqCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	// B1：随后被改坏的执行中批次；B2：始终完整的执行中批次。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("f-b1-1", "B1", "M1", "10", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f-b1-2", "B1", "M1", "20", fixedTime, "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "王五"); err != nil {
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
			// 只调换两条投料的位置而不同步序号：保存顺序的序号变成 2、1，
			// 物料归属与数量仍全部合法，唯一问题是序号与位置不一致。
			b.Feedings[0], b.Feedings[1] = b.Feedings[1], b.Feedings[0]
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询损坏批次：失败且不返回部分视图。
	if v, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("改坏后查询 B1 应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
	}
	// 查询完整批次、配方同样不能绕过。
	if v, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询完整批次 B2 也应失败，得到 %v", err)
	} else if v != nil {
		t.Fatalf("整份台账损坏时不应返回 B2 的部分视图: %+v", v)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("改坏后查询配方也应返回 ErrCorruptData，得到 %v", err)
	}
	// 对完整批次投料、关闭，以及对损坏批次继续追加投料，都不能绕过。
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "赵六"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对完整批次投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-bad-batch", "B1", "M1", "1", time.Now(), "赵六"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对损坏批次继续投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭完整批次也应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不留业务变化：文件内容不变，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	for _, marker := range []string{"feed-rejected", "feed-bad-batch", "close-rejected"} {
		if bytes.Contains(after, []byte(marker)) {
			t.Fatalf("被拒绝的写入不应留下请求记录 %q", marker)
		}
	}

	// 恢复原内容后：两条投料登记顺序完整，原成功请求可幂等重放，
	// 被拒绝过的请求编号可以正常使用并取得后续连续序号。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询 B1 应成功: %v", err)
	}
	if len(b1.Feedings) != 2 || b1.Feedings[0].Seq != 1 || b1.Feedings[0].Grams != "10" ||
		b1.Feedings[1].Seq != 2 || b1.Feedings[1].Grams != "20" {
		t.Fatalf("B1 原有投料顺序应完整保留: %+v", b1.Feedings)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询 B2 应成功: %v", err)
	}
	if b2.Status != StatusExecuting {
		t.Fatalf("被拒绝的关闭不应改变 B2 状态，得到 %s", b2.Status)
	}
	if _, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "王五"); err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "赵六")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}
