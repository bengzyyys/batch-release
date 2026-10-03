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

// 手工构造一份含指定投料记录的台账文件。
func writeFeedingState(t *testing.T, dir string, feedings []feedingRecord) []byte {
	t.Helper()
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1()},
		Batches: []*batchRecord{{
			BatchNo:         "B1",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: 2,
			Status:          StatusExecuting,
			Feedings:        feedings,
		}},
	})
}

// 已保存的投料为零或负数时，Open 必须返回 ErrCorruptData：
// 错误信息指出批次编号、物料编号与登记序号，并与累计超限的原因区分开；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsNonPositiveFeeding(t *testing.T) {
	cases := []struct {
		name  string
		grams gramsMilli
	}{
		{"零", 0},
		{"负数", -500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeFeedingState(t, dir, []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: time.Now(), Registrar: "张三"},
				{Seq: 2, MaterialNo: "M1", GramsMilli: tc.grams, Time: time.Now(), Registrar: "张三"},
			})

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("投料数量不是正数应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"B1", "M1", "2"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			if !strings.Contains(msg, "不是正数") {
				t.Fatalf("错误信息应区分单条数量非法，得到 %v", err)
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

// 负数记录即使与正数相加后落在范围内，也不能被接受。
func TestOpenRejectsNegativeFeedingEvenWhenNetted(t *testing.T) {
	dir := t.TempDir()
	writeFeedingState(t, dir, []feedingRecord{
		{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: time.Now(), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M1", GramsMilli: -400, Time: time.Now(), Registrar: "张三"},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("正负相抵后落在范围内仍应拒绝，得到 %v", err)
	}
}

// 同一批次同一物料累计实投超过上限时，Open 必须返回 ErrCorruptData：
// 错误信息指出批次编号与物料编号，并与单条非正数的原因区分开。
func TestOpenRejectsCumulativeOverLimit(t *testing.T) {
	dir := t.TempDir()
	writeFeedingState(t, dir, []feedingRecord{
		{Seq: 1, MaterialNo: "M1", GramsMilli: gramsMilli(math.MaxInt64), Time: time.Now(), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M1", GramsMilli: 1, Time: time.Now(), Registrar: "张三"},
	})
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("累计超限应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"B1", "M1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
	if !strings.Contains(msg, "超出上限") {
		t.Fatalf("错误信息应区分累计超限，得到 %v", err)
	}
}

// 累计恰好等于上限合法；上限按批次内物料分别判断，
// 两种物料各自达到上限不因合计更大而被拒绝。
func TestOpenAcceptsLimitExactlyAndPerMaterial(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{{
			RecipeNo: "R1", Version: "v1", Name: "配方一",
			Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
				{MaterialNo: "M2", GramsMilli: 500},
			},
		}},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: gramsMilli(math.MaxInt64), Time: time.Now(), Registrar: "张三"},
				{Seq: 2, MaterialNo: "M2", GramsMilli: gramsMilli(math.MaxInt64), Time: time.Now(), Registrar: "李四"},
			},
		}},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("各自恰好达到上限应正常打开: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m.ActualGrams
	}
	if got["M1"] != limitGrams || got["M2"] != limitGrams {
		t.Fatalf("累计实投应各自等于上限，得到 %v", got)
	}
}

// 台账打开后保存内容变成投料数量非法：下一次查询或写入同样失败，
// 不能沿用此前正常的内容；查询或修改正常批次也不能绕过；
// 被拒绝的写入不保存业务变更、不占用请求编号，原台账内容不变。
func TestFeedingCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-ok", "B2", "M1", "5", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	// 把 B1 的保存内容改坏：加入一条负数投料（B2 仍完整）。
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
				Seq: 1, MaterialNo: "M1", GramsMilli: -1, Time: fixedTime, Registrar: "张三",
			})
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询损坏批次与正常批次都必须失败。
	for _, batchNo := range []string{"B1", "B2"} {
		if _, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
	}
	// 写入正常批次同样不能绕过。
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
	if bytes.Contains(after, []byte("feed-rejected")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后原记录完整可用，被拒绝过的请求编号仍可合法提交。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
		t.Fatalf("原有投料记录应保留: %+v", b2)
	}
	f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}
