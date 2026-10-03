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

func recipeR1v1TwoMaterials() *recipeRecord {
	return &recipeRecord{RecipeNo: "R1", Version: "v1", Name: "配方一",
		Materials: []materialRecord{
			{MaterialNo: "M1", GramsMilli: 100000},
			{MaterialNo: "M2", GramsMilli: 500},
		}}
}

// 打开台账时，已保存投料为零或负数必须判定整份台账损坏：
// 三种批次状态一视同仁；错误信息指出批次编号、物料编号和该条登记序号，
// 并说明原因是单条投料不是正数；不返回可用的 Store，也不改写原文件。
func TestOpenRejectsNonPositiveFeeding(t *testing.T) {
	cases := []struct {
		name  string
		milli gramsMilli
	}{
		{"zero", 0},
		{"negative", -5000},
	}
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		for _, c := range cases {
			t.Run(string(status)+"/"+c.name, func(t *testing.T) {
				dir := t.TempDir()
				original := writeStateFile(t, dir, &persistedState{
					Version: stateVersion,
					Recipes: []*recipeRecord{recipeR1v1()},
					Batches: []*batchRecord{{
						BatchNo:         "B-bad",
						RecipeNo:        "R1",
						RecipeVersion:   "v1",
						PlannedPortions: 2,
						Status:          status,
						Feedings: []feedingRecord{
							{Seq: 1, MaterialNo: "M1", GramsMilli: 6000},
							{Seq: 2, MaterialNo: "M1", GramsMilli: c.milli},
						},
					}},
				})

				s, err := Open(dir)
				if !errors.Is(err, ErrCorruptData) {
					t.Fatalf("存在非正投料应返回 ErrCorruptData，得到 %v", err)
				}
				if s != nil {
					s.Close()
					t.Fatalf("损坏台账不应返回可用的 Store 对象")
				}
				msg := err.Error()
				for _, want := range []string{"B-bad", "M1", "第 2 条", "不是正数"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
					}
				}
				if strings.Contains(msg, "超过上限") {
					t.Fatalf("单条数量非法应与累计超限区分开，得到 %v", err)
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

// 每条记录本身都是正数，但同一物料累计超过上限，同样是损坏：
// 错误信息指出批次编号与物料编号，原因是累计超过上限（而非单条非正）。
func TestOpenRejectsCumulativeOverLimitFeeding(t *testing.T) {
	cases := []struct {
		name     string
		feedings []feedingRecord
	}{
		{
			name: "limit-then-one-milli",
			feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: maxGramsMilli},
				{Seq: 2, MaterialNo: "M1", GramsMilli: 1},
			},
		},
		{
			name: "two-positive-records-overflow",
			feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: maxGramsMilli - 100},
				{Seq: 2, MaterialNo: "M1", GramsMilli: 200},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeStateFile(t, dir, &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{recipeR1v1()},
				Batches: []*batchRecord{{
					BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 1, Status: StatusExecuting, Feedings: c.feedings,
				}},
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
			for _, want := range []string{"B-bad", "M1", "累计实投超过上限", limitGrams} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			if strings.Contains(msg, "不是正数") {
				t.Fatalf("累计超限应与单条非正区分开，得到 %v", err)
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

// 负数与正数相加后即使落在范围内，负数记录也不能被接受
// （不能靠后面的正数把累计“冲回”合法区间）；错误需指出负数那条的登记序号。
func TestOpenNegativeFeedingNotOffsetByPositive(t *testing.T) {
	cases := []struct {
		name     string
		badSeq   int
		feedings []feedingRecord
	}{
		{"negative-first", 1, []feedingRecord{
			{Seq: 1, MaterialNo: "M1", GramsMilli: -5000},
			{Seq: 2, MaterialNo: "M1", GramsMilli: 6000},
		}},
		{"negative-last", 2, []feedingRecord{
			{Seq: 1, MaterialNo: "M1", GramsMilli: 6000},
			{Seq: 2, MaterialNo: "M1", GramsMilli: -5000},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeStateFile(t, dir, &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{recipeR1v1()},
				Batches: []*batchRecord{{
					BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 1, Status: StatusExecuting, Feedings: c.feedings,
				}},
			})
			_, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("正负相加落在范围内也必须拒绝，得到 %v", err)
			}
			msg := err.Error()
			for _, want := range []string{"B1", "M1", "不是正数", "第 " + strconv.Itoa(c.badSeq) + " 条"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			if strings.Contains(msg, "超过上限") {
				t.Fatalf("负数记录应按单条非正拒绝，而不是按累计超限，得到 %v", err)
			}
		})
	}
}

// 累计恰好等于上限接受；两种物料各自达到上限不能因合计更大而被拒绝。
func TestOpenFeedingAtLimitAcceptedPerMaterial(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1TwoMaterials()},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 1, Status: StatusExecuting,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: maxGramsMilli},
				{Seq: 2, MaterialNo: "M2", GramsMilli: maxGramsMilli},
			},
		}},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("两物料各自恰好达到上限应能打开: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("上限数据查询应成功: %v", err)
	}
	got := map[string]MaterialRequirement{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m
	}
	if got["M1"].ActualGrams != limitGrams || got["M2"].ActualGrams != limitGrams {
		t.Fatalf("两物料累计实投都应为上限，得到 %+v %+v", got["M1"], got["M2"])
	}
	if len(view.Feedings) != 2 || view.Feedings[0].Seq != 1 || view.Feedings[1].Seq != 2 {
		t.Fatalf("既有登记顺序应保留: %+v", view.Feedings)
	}
}

// 台账里同时存在正常批次也不能靠正常批次绕过：损坏批次存在时整份台账
// 拒绝打开；修复（去掉非法投料）后正常批次与原批次内容都继续可用。
func TestOpenOneBrokenFeedingBatchAmongCompleteOnes(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1TwoMaterials()},
		Batches: []*batchRecord{
			{BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed,
				Feedings: []feedingRecord{{Seq: 1, MaterialNo: "M1", GramsMilli: 7000}}},
			{BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusExecuting,
				Feedings: []feedingRecord{{Seq: 1, MaterialNo: "M2", GramsMilli: -1}}},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在正常批次也应整体拒绝，得到 %v", err)
	}

	// 修复：删除负数投料（恢复成与原登记一致的合法内容），其余记录原样保留。
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{recipeR1v1TwoMaterials()},
		Batches: []*batchRecord{
			{BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed,
				Feedings: []feedingRecord{{Seq: 1, MaterialNo: "M1", GramsMilli: 7000}}},
			{BatchNo: "B-bad", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusExecuting},
		},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("修复后重新打开应成功: %v", err)
	}
	defer s.Close()
	ok, err := s.GetBatch("B-ok")
	if err != nil {
		t.Fatal(err)
	}
	if ok.Status != StatusClosed || len(ok.Feedings) != 1 || ok.Feedings[0].Grams != "7" {
		t.Fatalf("已关闭批次的投料内容应原样保留: %+v", ok)
	}
	bad, err := s.GetBatch("B-bad")
	if err != nil {
		t.Fatal(err)
	}
	if bad.PlannedPortions != 2 || len(bad.Feedings) != 0 {
		t.Fatalf("修复批次内容不正确: %+v", bad)
	}
	if m := bad.Materials[0]; m.ActualGrams != "0" {
		t.Fatalf("没有投料的物料累计实投应为零且正常，得到 %s", m.ActualGrams)
	}
}

// 台账打开后保存内容被改成数量非法（零数投料或累计超限）：
// 下一次查询或写入都必须失败，不能沿用此前正常的内容；查询或修改
// 正常批次、查询配方都不能绕过；被拒绝的写入不改文件、不留业务记录、
// 不占用请求编号；恢复原内容后原有记录、登记顺序与幂等结果继续可用。
func TestFeedingCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	for _, kind := range []string{"zero", "overflow"} {
		t.Run(kind, func(t *testing.T) {
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
			if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartBatch("s1", "B1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateBatch("b2", "B2", "R2", "v1", 10); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartBatch("s2", "B2"); err != nil {
				t.Fatal(err)
			}
			fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
			if _, err := s.AddFeeding("feed-b1", "B1", "M1", "5", fixedTime, "张三"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddFeeding("feed-b2", "B2", "M1", "3", fixedTime, "李四"); err != nil {
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
				if b.BatchNo != "B1" {
					continue
				}
				switch kind {
				case "zero":
					b.Feedings[0].GramsMilli = 0
				case "overflow":
					b.Feedings = append(b.Feedings, feedingRecord{
						Seq: 2, MaterialNo: "M1", GramsMilli: maxGramsMilli,
					})
				}
			}
			badBytes := writeStateFile(t, dir, &broken)

			// 查询：损坏批次、正常批次、配方查询全部失败，不能沿用内存中的旧内容。
			for _, batchNo := range []string{"B1", "B2"} {
				if _, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
					t.Fatalf("文件损坏后查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
				}
			}
			if _, err := s.GetRecipe("R2", "v1"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("文件损坏后查询配方也应失败，得到 %v", err)
			}

			// 写入：对损坏批次与对正常批次的操作都必须被拒绝。
			if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "王五"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.CloseBatch("close-b2", "B2"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("不能借关闭正常批次绕过读取失败，得到 %v", err)
			}
			if _, err := s.CreateBatch("b3", "B3", "R2", "v1", 1); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上新建批次也应失败，得到 %v", err)
			}
			if _, err := s.RegisterRecipe("r3", "R3", "v1", "新配方", []MaterialInput{
				{MaterialNo: "M1", Grams: "1"},
			}); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上登记配方也应失败，得到 %v", err)
			}

			// 被拒绝的写入不留痕迹：文件内容不变，请求编号与业务记录都不存在。
			after, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, badBytes) {
				t.Fatalf("被拒绝的写入不应改动台账文件")
			}
			if bytes.Contains(after, []byte("feed-rejected")) || bytes.Contains(after, []byte("B3")) || bytes.Contains(after, []byte("R3")) {
				t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
			}

			// 恢复原内容后：原有投料、登记顺序保留，幂等重放与被拒编号都正常。
			if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
				t.Fatal(err)
			}
			b1, err := s.GetBatch("B1")
			if err != nil {
				t.Fatalf("恢复后查询应成功: %v", err)
			}
			if len(b1.Feedings) != 1 || b1.Feedings[0].Grams != "5" || b1.Feedings[0].Seq != 1 {
				t.Fatalf("原有投料与登记顺序应保留: %+v", b1.Feedings)
			}
			replay, err := s.AddFeeding("feed-b1", "B1", "M1", "5", fixedTime, "张三")
			if err != nil || replay.Seq != 1 || replay.Grams != "5" {
				t.Fatalf("恢复后重放原成功请求应返回原结果，得到 %+v, %v", replay, err)
			}
			f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "王五")
			if err != nil {
				t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
			}
			if f.Seq != 2 || f.Grams != "1" {
				t.Fatalf("被拒编号的新投料结果不正确: %+v", f)
			}
		})
	}
}

// 数量核对仍然只看实投与应投的差额：不足或超过应投量照常显示，
// 不会因为读取时新增的数量合法性检查而被拒绝或影响批次关闭。
func TestQuantityVarianceStillAllowedAfterReadValidation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RegisterRecipe("r", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s", "B1"); err != nil {
		t.Fatal(err)
	}
	// M1 欠投（应投 1000，实投 5），M2 超投（应投 5，实投 9），均合法；
	// 数量不吻合也不阻止关闭。
	if _, err := s.AddFeeding("f1", "B1", "M1", "5", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M2", "9", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("c", "B1"); err != nil {
		t.Fatalf("欠投/超投不应阻止关闭批次: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("有欠投/超投的台账应正常打开: %v", err)
	}
	defer s2.Close()
	view, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]MaterialRequirement{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m
	}
	if got["M1"].DifferenceGrams != "-995" || got["M2"].DifferenceGrams != "4" {
		t.Fatalf("欠投/超投差额应照常显示: %+v %+v", got["M1"], got["M2"])
	}
	if view.Status != StatusClosed {
		t.Fatalf("数量不吻合不应阻止关闭，得到 %s", view.Status)
	}
}
