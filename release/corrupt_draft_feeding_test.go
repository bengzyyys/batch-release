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

// 手工构造一份含指定批次记录的台账文件（配方固定为 R1/v1：M1=100 克/份）。
func writeBatchState(t *testing.T, dir string, batches ...*batchRecord) []byte {
	t.Helper()
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1()},
		Batches: batches,
	})
}

// 草稿批次带有投料记录时，Open 必须返回 ErrCorruptData：
// 即使投料的物料都属于绑定版本、克数均为合法正数、累计实投恰好等于
// 应投，也不能接受。错误信息指出批次编号并说明草稿状态下存在投料记录，
// 与配方缺失、投料数量非法的原因区分开；不返回可用的台账对象，
// 也不改写原文件。台账里另有正常批次也不能放行。
func TestOpenRejectsDraftWithFeedings(t *testing.T) {
	cases := []struct {
		name     string
		feedings []feedingRecord
	}{
		{"单条投料", []feedingRecord{
			{Seq: 1, MaterialNo: "M1", GramsMilli: 50000, Time: time.Now(), Registrar: "张三"},
		}},
		{"累计恰好等于应投", []feedingRecord{
			{Seq: 1, MaterialNo: "M1", GramsMilli: 100000, Time: time.Now(), Registrar: "张三"},
			{Seq: 2, MaterialNo: "M1", GramsMilli: 100000, Time: time.Now(), Registrar: "李四"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeBatchState(t, dir,
				&batchRecord{
					BatchNo:         "B1",
					RecipeNo:        "R1",
					RecipeVersion:   "v1",
					PlannedPortions: 2,
					Status:          StatusDraft,
					Feedings:        tc.feedings,
				},
				// 另有一个完全正常的执行中批次，不能因此放行。
				&batchRecord{
					BatchNo:         "B2",
					RecipeNo:        "R1",
					RecipeVersion:   "v1",
					PlannedPortions: 1,
					Status:          StatusExecuting,
					Feedings: []feedingRecord{
						{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: time.Now(), Registrar: "张三"},
					},
				},
			)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("草稿带有投料应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			if !strings.Contains(msg, "B1") {
				t.Fatalf("错误信息应指出批次编号 B1，得到 %v", err)
			}
			if !strings.Contains(msg, "草稿") || !strings.Contains(msg, "投料") {
				t.Fatalf("错误信息应说明草稿状态下存在投料记录，得到 %v", err)
			}
			for _, other := range []string{"未登记", "不是正数", "超出上限"} {
				if strings.Contains(msg, other) {
					t.Fatalf("错误信息不应与配方缺失或数量非法混淆（含 %q），得到 %v", other, err)
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

// 台账打开后保存内容变成“草稿带投料”：下一次查询或带有效请求编号的
// 写入同样失败；查询正常批次、查询配方也不能绕过；被拒绝的写入不保存
// 业务变更、不占用请求编号，原台账内容不变。
func TestDraftFeedingCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-ok", "B2", "M1", "5", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	// 把 B1 的保存内容改坏：草稿状态下加入一条合法投料（B2 仍完整）。
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
			b.Feedings = append(b.Feedings, feedingRecord{
				Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: fixedTime, Registrar: "张三",
			})
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询损坏批次、正常批次与配方都必须失败。
	for _, batchNo := range []string{"B1", "B2"} {
		if _, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("查询配方应返回 ErrCorruptData，得到 %v", err)
	}
	// 带有效请求编号的写入同样不能绕过。
	if _, err := s.UpdateDraftBatch("u-rejected", "B1", "", "", 5); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整草稿应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	if bytes.Contains(after, []byte("u-rejected")) || bytes.Contains(after, []byte("feed-rejected")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后原记录完整可用，被拒绝过的请求编号仍可合法提交。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if b1.Status != StatusDraft || len(b1.Feedings) != 0 {
		t.Fatalf("原有草稿应无投料: %+v", b1)
	}
	adjusted, err := s.UpdateDraftBatch("u-rejected", "B1", "", "", 5)
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if adjusted.PlannedPortions != 5 {
		t.Fatalf("调整结果不正确: %+v", adjusted)
	}
}

// 本次约束只针对“草稿已有投料”：没有投料的草稿仍可正常查询和调整；
// 执行中尚未投料、已关闭批次没有投料或仍有欠投、超投都属正常，
// 不能借本次修正提高关闭门槛。合法草稿的数量核对仍列出全部配方物料，
// 累计实投为零，差额为应投量的负值。
func TestLegitBatchesWithoutDraftFeedingsStillAccepted(t *testing.T) {
	dir := t.TempDir()
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	writeBatchState(t, dir,
		// 无投料的草稿。
		&batchRecord{
			BatchNo: "B-draft", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusDraft,
		},
		// 执行中、尚未投料。
		&batchRecord{
			BatchNo: "B-exec", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusExecuting,
		},
		// 已关闭、没有投料。
		&batchRecord{
			BatchNo: "B-closed-empty", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusClosed,
		},
		// 已关闭、欠投（应投 100，实投 40）。
		&batchRecord{
			BatchNo: "B-closed-under", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusClosed,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 40000, Time: fixedTime, Registrar: "张三"},
			},
		},
		// 已关闭、超投（应投 100，实投 150）。
		&batchRecord{
			BatchNo: "B-closed-over", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusClosed,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 150000, Time: fixedTime, Registrar: "张三"},
			},
		},
	)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("无草稿投料的台账应正常打开: %v", err)
	}
	defer s.Close()

	// 合法草稿：数量核对列出全部配方物料，实投为零，差额为应投的负值。
	draft, err := s.GetBatch("B-draft")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Status != StatusDraft || len(draft.Feedings) != 0 {
		t.Fatalf("草稿状态或投料不正确: %+v", draft)
	}
	if len(draft.Materials) != 1 {
		t.Fatalf("草稿应列出全部配方物料，得到 %d 项", len(draft.Materials))
	}
	m := draft.Materials[0]
	if m.MaterialNo != "M1" || m.RequiredGrams != "200" || m.ActualGrams != "0" || m.DifferenceGrams != "-200" {
		t.Fatalf("草稿数量核对不正确: %+v", m)
	}
	// 无投料的草稿仍可调整份数。
	adjusted, err := s.UpdateDraftBatch("u1", "B-draft", "", "", 3)
	if err != nil {
		t.Fatalf("无投料的草稿应可调整: %v", err)
	}
	if adjusted.PlannedPortions != 3 || adjusted.Status != StatusDraft {
		t.Fatalf("调整结果不正确: %+v", adjusted)
	}

	// 执行中尚未投料、已关闭无投料都正常。
	for _, batchNo := range []string{"B-exec", "B-closed-empty"} {
		view, err := s.GetBatch(batchNo)
		if err != nil {
			t.Fatalf("查询 %q 失败: %v", batchNo, err)
		}
		if len(view.Feedings) != 0 || view.Materials[0].ActualGrams != "0" {
			t.Fatalf("%q 应无投料: %+v", batchNo, view)
		}
	}
	// 已关闭批次的欠投、超投原样保留，关闭门槛不提高。
	under, err := s.GetBatch("B-closed-under")
	if err != nil {
		t.Fatal(err)
	}
	if under.Materials[0].ActualGrams != "40" || under.Materials[0].DifferenceGrams != "-60" {
		t.Fatalf("欠投应原样保留: %+v", under.Materials[0])
	}
	over, err := s.GetBatch("B-closed-over")
	if err != nil {
		t.Fatal(err)
	}
	if over.Materials[0].ActualGrams != "150" || over.Materials[0].DifferenceGrams != "50" {
		t.Fatalf("超投应原样保留: %+v", over.Materials[0])
	}
}
