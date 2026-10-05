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

// 本文件是“已保存投料的登记序号必须与记录位置一致”的回归保障：
// 序号在写入时按成功登记的先后次序从 1 连续生成，这里锁定读取侧——
// 打开台账及之后的每次查询/写入重载，都必须重新核对保存序号与记录在
// 列表中的位置，不允许序号重复、跳号或只调换位置而未同步序号；同时
// 保留跨物料共用批次序列、跨批次各自编号、时间不参与排序等公开行为。

// 构造含 R1/v1（M1、M2 两种物料）与一个批次 B1 的台账；投料按给定
// 记录原样写入，序号也由用例指定，构造过程不做任何“修正”。
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
			BatchNo:         "B1",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: 2,
			Status:          status,
			Feedings:        feedings,
		}},
	})
}

func seqFeeding(seq int, material string) feedingRecord {
	return feedingRecord{
		Seq:        seq,
		MaterialNo: material,
		GramsMilli: 1000,
		Time:       time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		Registrar:  "张三",
	}
}

// 已保存投料的序号不从 1 开始、为零或负数、重号、漏号、跳号，或只调换
// 记录位置而未同步序号时，Open 必须返回 ErrCorruptData——即使物料全部
// 属于绑定版本、数量均为合法正数也不能放行。错误信息必须指出问题批次
// 编号、该记录在列表中的位置、实际序号与应有序号；不得返回可用的 Store，
// 也不得改写原文件。执行中与已关闭批次同等处理（关闭后不可追加，但历史
// 序号一样受完整性约束）。
func TestOpenRejectsFeedingSeqOutOfOrder(t *testing.T) {
	cases := []struct {
		name    string
		mats    []string
		seqs    []int
		pos     int // 第一条失去顺序的记录位置（从 1 起）
		actual  int // 该位置上的实际序号
		wantSeq int // 该位置应有的序号
	}{
		{"三条记录序号为 1、1、3（重号）", []string{"M1", "M2", "M1"}, []int{1, 1, 3}, 2, 1, 2},
		{"三条记录序号为 1、3、2（换位未同步序号）", []string{"M1", "M2", "M1"}, []int{1, 3, 2}, 2, 3, 2},
		{"第一条序号不是 1", []string{"M1"}, []int{2}, 1, 2, 1},
		{"第一条序号为零", []string{"M1"}, []int{0}, 1, 0, 1},
		{"第一条序号为负数", []string{"M1"}, []int{-1}, 1, -1, 1},
		{"跳号 1、3", []string{"M1", "M2"}, []int{1, 3}, 2, 3, 2},
		{"漏号 1、2、4", []string{"M1", "M2", "M1"}, []int{1, 2, 4}, 3, 4, 3},
		{"末尾重号 1、2、2", []string{"M1", "M2", "M2"}, []int{1, 2, 2}, 3, 2, 3},
	}
	for _, status := range []BatchStatus{StatusExecuting, StatusClosed} {
		for _, tc := range cases {
			t.Run(string(status)+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				feedings := make([]feedingRecord, len(tc.seqs))
				for i, seq := range tc.seqs {
					feedings[i] = seqFeeding(seq, tc.mats[i])
				}
				original := writeSeqState(t, dir, status, feedings)

				s, err := Open(dir)
				if !errors.Is(err, ErrCorruptData) {
					if s != nil {
						s.Close()
					}
					t.Fatalf("序号失去顺序应返回 ErrCorruptData，得到 %v", err)
				}
				if s != nil {
					s.Close()
					t.Fatalf("损坏台账不应返回可用的 Store 对象")
				}
				msg := err.Error()
				for _, want := range []string{
					"B1",
					"第 " + strconv.Itoa(tc.pos) + " 条",
					strconv.Itoa(tc.actual),
					strconv.Itoa(tc.wantSeq),
				} {
					if !strings.Contains(msg, want) {
						t.Fatalf("错误信息应包含 %q（批次、位置、实际序号、应有序号），得到 %v", want, err)
					}
				}
				// 读取失败不得重排、补号或删除投料。
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

// 序号是整个批次共用的一条登记序列：不同物料的投料依次共用 1、2、3……
// 不按物料各自从 1 编号。M1、M2 交错登记且序号连续时必须正常读取；
// 若第二种物料自行从 1 编号，则属于重号损坏。
func TestSeqSharedAcrossMaterialsNotPerMaterial(t *testing.T) {
	t.Run("跨物料连续序号合法", func(t *testing.T) {
		dir := t.TempDir()
		writeSeqState(t, dir, StatusExecuting, []feedingRecord{
			seqFeeding(1, "M1"),
			seqFeeding(2, "M2"),
			seqFeeding(3, "M1"),
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("跨物料共用连续序号应正常打开: %v", err)
		}
		defer s.Close()
		view, err := s.GetBatch("B1")
		if err != nil {
			t.Fatal(err)
		}
		got := []struct {
			seq int
			mat string
		}{}
		for _, f := range view.Feedings {
			got = append(got, struct {
				seq int
				mat string
			}{f.Seq, f.MaterialNo})
		}
		want := []struct {
			seq int
			mat string
		}{{1, "M1"}, {2, "M2"}, {3, "M1"}}
		if len(got) != len(want) {
			t.Fatalf("投料条数不正确: got=%+v want=%+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("第 %d 条投料应保持登记位置与序号: got=%+v want=%+v", i+1, got[i], want[i])
			}
		}
		// 执行中的合法批次仍可继续追加，新投料序号接续整条批次序列。
		f, err := s.AddFeeding("feed-next", "B1", "M2", "2", time.Now(), "李四")
		if err != nil {
			t.Fatalf("合法批次应能继续追加投料: %v", err)
		}
		if f.Seq != 4 {
			t.Fatalf("新投料序号应为 4，得到 %d", f.Seq)
		}
	})

	t.Run("按物料各自编号属于重号损坏", func(t *testing.T) {
		dir := t.TempDir()
		original := writeSeqState(t, dir, StatusExecuting, []feedingRecord{
			seqFeeding(1, "M1"),
			seqFeeding(1, "M2"), // M2 不能自行从 1 开始
		})
		s, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("不同物料共用批次序列，重号应返回 ErrCorruptData，得到 %v", err)
		}
		msg := err.Error()
		for _, want := range []string{"B1", "第 2 条", "1", "2"} {
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

// 不同批次各自从 1 开始编号：两个批次的序号都从 1 起连续即为合法，
// 不要求跨批次全局唯一；其中一个批次序号损坏时，即使另一个批次完整，
// 整份台账仍要拒绝。
func TestSeqIndependentPerBatch(t *testing.T) {
	t.Run("两个批次各自从 1 连续编号合法", func(t *testing.T) {
		dir := t.TempDir()
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
				{
					BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 2, Status: StatusExecuting,
					Feedings: []feedingRecord{
						seqFeeding(1, "M1"),
						seqFeeding(2, "M2"),
						seqFeeding(3, "M1"),
					},
				},
				{
					BatchNo: "B2", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 1, Status: StatusClosed,
					Feedings: []feedingRecord{
						seqFeeding(1, "M2"),
						seqFeeding(2, "M1"),
					},
				},
			},
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("各批次独立编号应正常打开: %v", err)
		}
		defer s.Close()
		b1, err := s.GetBatch("B1")
		if err != nil {
			t.Fatal(err)
		}
		if len(b1.Feedings) != 3 || b1.Feedings[2].Seq != 3 {
			t.Fatalf("B1 序号应为 1、2、3: %+v", b1.Feedings)
		}
		b2, err := s.GetBatch("B2")
		if err != nil {
			t.Fatal(err)
		}
		if len(b2.Feedings) != 2 || b2.Feedings[0].Seq != 1 || b2.Feedings[1].Seq != 2 {
			t.Fatalf("B2 应独立从 1 编号: %+v", b2.Feedings)
		}
	})

	t.Run("一个批次序号损坏不能靠另一个正常批次放行", func(t *testing.T) {
		dir := t.TempDir()
		writeStateFile(t, dir, &persistedState{
			Version: stateVersion,
			Recipes: []*recipeRecord{{
				RecipeNo: "R1", Version: "v1", Name: "配方一",
				Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}},
			}},
			Batches: []*batchRecord{
				{
					BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 2, Status: StatusExecuting,
					Feedings: []feedingRecord{
						seqFeeding(1, "M1"),
						seqFeeding(1, "M1"), // 重号
					},
				},
				{
					BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 1, Status: StatusExecuting,
					Feedings: []feedingRecord{seqFeeding(1, "M1")},
				},
			},
		})
		s, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("存在损坏批次时整份台账都不能打开，得到 %v", err)
		}
		if s != nil {
			s.Close()
			t.Fatalf("损坏台账不应返回可用的 Store 对象")
		}
		if !strings.Contains(err.Error(), "B-bad") {
			t.Fatalf("错误信息应指出问题批次 B-bad，得到 %v", err)
		}
	})
}

// 空投料列表不构成序号错误：草稿、执行中、已关闭三种状态下没有投料都
// 正常，仍按原有的批次状态规则处理（执行中可追加，关闭后不可追加）。
func TestEmptyFeedingsHaveNoSeqError(t *testing.T) {
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			writeSeqState(t, dir, status, nil)
			s, err := Open(dir)
			if err != nil {
				t.Fatalf("空投料列表不应报序号错误: %v", err)
			}
			defer s.Close()
			view, err := s.GetBatch("B1")
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Feedings) != 0 {
				t.Fatalf("应没有投料记录，得到 %d 条", len(view.Feedings))
			}
		})
	}
}

// 投料时间从不参与排序：两条投料时间完全相同，或后一条填写的时间早于
// 前一条，登记位置与序号都保持成功登记的先后次序，重新读取同样合法。
func TestFeedingTimeNeverAffectsSeq(t *testing.T) {
	dir := t.TempDir()
	early := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	later := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	same := time.Date(2026, 2, 2, 9, 0, 0, 0, time.UTC)
	writeSeqState(t, dir, StatusExecuting, []feedingRecord{
		{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: later, Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", GramsMilli: 1000, Time: early, Registrar: "李四"}, // 时间早于前一条
		{Seq: 3, MaterialNo: "M1", GramsMilli: 1000, Time: same, Registrar: "王五"},
		{Seq: 4, MaterialNo: "M2", GramsMilli: 1000, Time: same, Registrar: "赵六"}, // 与前一条时间相同
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("时间相同或乱序不能用来判断序号损坏: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq := []int{1, 2, 3, 4}
	wantMat := []string{"M1", "M2", "M1", "M2"}
	for i, f := range view.Feedings {
		if f.Seq != wantSeq[i] || f.MaterialNo != wantMat[i] {
			t.Fatalf("第 %d 条应按登记位置保留，得到 %+v", i+1, f)
		}
	}
}

// 序号问题可以且只能通过让序号与记录位置重新一致来修复：系统不自行
// 重排或补号；人工修正保存内容后重新打开，全部原记录（含物料、数量、
// 时间、登记人）完整保留。
func TestSeqCorruptionRecoveredByRealigningSeq(t *testing.T) {
	dir := t.TempDir()
	original := writeSeqState(t, dir, StatusExecuting, []feedingRecord{
		seqFeeding(1, "M1"),
		seqFeeding(3, "M2"), // 跳号
		seqFeeding(2, "M1"), // 漏号/换位
	})
	if s, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("序号 1、3、2 应拒绝，得到 %v", err)
	}

	// 人工把序号改成与保存位置一致（不增删记录、不调换位置），重新打开。
	var repaired persistedState
	if err := json.Unmarshal(original, &repaired); err != nil {
		t.Fatal(err)
	}
	for _, b := range repaired.Batches {
		if b.BatchNo == "B1" {
			b.Feedings[1].Seq = 2
			b.Feedings[2].Seq = 3
		}
	}
	writeStateFile(t, dir, &repaired)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("序号与位置一致后应能打开: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 3 {
		t.Fatalf("三条原记录都应保留，得到 %d 条", len(view.Feedings))
	}
	for i, f := range view.Feedings {
		if f.Seq != i+1 {
			t.Fatalf("第 %d 条序号应为 %d，得到 %+v", i+1, i+1, f)
		}
	}
	if view.Feedings[1].MaterialNo != "M2" || view.Feedings[2].MaterialNo != "M1" {
		t.Fatalf("记录位置与物料归属应保持原样: %+v", view.Feedings)
	}
}

// 台账正常打开后，保存内容中的投料序号被改坏（只调换两条记录的位置、
// 不改正序号）：下一次查询或写入重载时必须返回 ErrCorruptData，查询
// 损坏批次或其他正常批次、查询配方、对正常批次投料或关闭都不能绕过；
// 已成功的旧请求编号在损坏状态下也不能重放。被拒绝的写入不留业务变化、
// 不占用请求编号，原文件保持不变；恢复序号后原有记录完整、被拒绝过的
// 请求编号仍可合法提交并取得连续序号。
func TestSeqCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "50"},
	}); err != nil {
		t.Fatal(err)
	}
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
	// B1 两条不同物料的合法投料：seq 1=M1，seq 2=M2。
	if _, err := s.AddFeeding("f-b1a", "B1", "M1", "100", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f-b1b", "B1", "M2", "7", fixedTime, "李四"); err != nil {
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
			// 只调换两条记录的保存位置、保留各自序号：
			// 列表变为 [seq2/M2, seq1/M1]，物料归属与数量仍全部合法。
			b.Feedings[0], b.Feedings[1] = b.Feedings[1], b.Feedings[0]
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询损坏批次与正常批次都必须失败，且不返回部分视图。
	if v, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("改坏后查询 B1 应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的查询不应返回部分视图: %+v", v)
	}
	if v, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询正常批次 B2 也应失败，得到 %v", err)
	} else if v != nil {
		t.Fatalf("整份台账损坏时不应返回 B2 的部分视图: %+v", v)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("改坏后查询配方也应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次投料、关闭正常批次都不能绕过读取检查。
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "赵六"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对正常批次投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-bad-batch", "B1", "M1", "1", time.Now(), "赵六"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上对损坏批次投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭正常批次也应返回 ErrCorruptData，得到 %v", err)
	}
	// 此前成功的请求编号在损坏状态下同样无法重放（重放前必须先重读整份台账）。
	if _, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "王五"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上旧成功请求也不能重放，得到 %v", err)
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
	if _, err := s.GetBatch("B1"); err == nil {
		t.Fatal("损坏台账上查询应返回错误")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "B1") || !strings.Contains(msg, "第 1 条") {
			t.Fatalf("错误信息应能定位问题批次与位置，得到 %q", msg)
		}
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("应返回 ErrCorruptData，得到 %v", err)
		}
	}

	// 恢复原内容后：两条合法投料顺序完整，正常批次状态不变，原成功请求
	// 可幂等重放，被拒绝过的请求编号可以正常使用并取得连续序号。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询 B1 应成功: %v", err)
	}
	if len(b1.Feedings) != 2 || b1.Feedings[0].Seq != 1 || b1.Feedings[0].MaterialNo != "M1" ||
		b1.Feedings[1].Seq != 2 || b1.Feedings[1].MaterialNo != "M2" {
		t.Fatalf("B1 应保持原登记顺序: %+v", b1.Feedings)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询 B2 应成功: %v", err)
	}
	if b2.Status != StatusExecuting || len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
		t.Fatalf("B2 状态与投料应保持不变: %+v", b2)
	}
	replay, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "王五")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "赵六")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}
