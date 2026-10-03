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

// Open 时任一配方版本的任一物料每份克数为零或负数，都必须返回
// ErrCorruptData：不论该版本是否被批次使用；错误信息需包含配方编号、
// 版本号与物料编号；不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsNonPositiveRecipeGrams(t *testing.T) {
	cases := []struct {
		name    string
		recipes []*recipeRecord
		batches []*batchRecord
		want    []string // 错误信息必须包含的片段
	}{
		{
			name: "被批次使用的版本含零克数",
			recipes: []*recipeRecord{
				{RecipeNo: "R1", Version: "v1", Name: "配方一",
					Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 0}}},
			},
			batches: []*batchRecord{{
				BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 2, Status: StatusDraft,
			}},
			want: []string{"R1", "v1", "M1"},
		},
		{
			name: "被批次使用的版本含负克数",
			recipes: []*recipeRecord{
				{RecipeNo: "R1", Version: "v1", Name: "配方一",
					Materials: []materialRecord{{MaterialNo: "M2", GramsMilli: -500}}},
			},
			batches: []*batchRecord{{
				BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 2, Status: StatusExecuting,
			}},
			want: []string{"R1", "v1", "M2"},
		},
		{
			name: "未被任何批次使用的版本含零克数",
			recipes: []*recipeRecord{
				recipeR1v1(),
				{RecipeNo: "R2", Version: "v3", Name: "配方二",
					Materials: []materialRecord{{MaterialNo: "M9", GramsMilli: 0}}},
			},
			batches: []*batchRecord{{
				BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusClosed,
			}},
			want: []string{"R2", "v3", "M9"},
		},
		{
			name: "多物料版本中仅一种物料非法",
			recipes: []*recipeRecord{
				{RecipeNo: "R1", Version: "v2", Name: "配方一",
					Materials: []materialRecord{
						{MaterialNo: "M1", GramsMilli: 100000},
						{MaterialNo: "M2", GramsMilli: -1},
					}},
			},
			want: []string{"R1", "v2", "M2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeStateFile(t, dir, &persistedState{
				Version: stateVersion,
				Recipes: tc.recipes,
				Batches: tc.batches,
			})

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("配方每份克数非正数应返回 ErrCorruptData，得到 %v", err)
			}
			if errors.Is(err, ErrInvalidInput) {
				t.Fatalf("已保存数据损坏不应报成提交参数错误，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
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

// 已有正数投料、批次数量恰好吻合，都不能让含非法用量的配方成为合法依据：
// Open 仍必须整体失败。
func TestOpenRejectsBadRecipeGramsDespiteMatchingBatch(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R1", Version: "v1", Name: "配方一",
				Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: -100}}},
		},
		Batches: []*batchRecord{{
			BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
			PlannedPortions: 2, Status: StatusClosed,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 100, Time: time.Now(), Registrar: "张三"},
				{Seq: 2, MaterialNo: "M1", GramsMilli: 100, Time: time.Now(), Registrar: "张三"},
			},
		}},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("正数投料与吻合数量不能挽救非法配方用量，得到 %v", err)
	}
}

// 台账打开后保存内容才出现非法配方用量：下一次配方查询、批次查询或写入
// 都必须返回 ErrCorruptData，不得返回部分结果或沿用此前的正确用量，
// 写入不得在损坏内容上登记新记录；被拒绝的写入不改动文件、不占用请求编号；
// 恢复正确用量后原记录继续可用。
func TestCorruptRecipeGramsAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("f1", "B1", "M1", "5", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	// 把已保存的每份克数改成零（不经过台账接口，模拟外部破坏）。
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	broken.Recipes[0].Materials[0].GramsMilli = 0
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：配方与批次都不得返回部分结果或沿用此前的正确用量。
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后配方查询应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后批次查询应返回 ErrCorruptData，得到 %v", err)
	}

	// 写入：登记配方、投料、状态操作都必须被拒绝。
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("f-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("c1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的操作不留痕迹：文件内容不变，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	if bytes.Contains(after, []byte("f-rejected")) || bytes.Contains(after, []byte("R2")) {
		t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
	}

	// 恢复正确用量后，原记录完整可用；被拒绝过的请求编号仍可合法使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("恢复后配方查询应成功: %v", err)
	}
	if len(r.Materials) != 1 || r.Materials[0].Grams != "100" {
		t.Fatalf("恢复后配方用量应为原值: %+v", r)
	}
	f, err := s.AddFeeding("f-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 正常数据不受新校验影响：0.001 克这样的正数合法；没有投料时累计实投
// 为零、欠投时差额为负，都不是非法配方用量。
func TestPositiveFractionGramsAndZeroActualStillValid(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.001"},
	}); err != nil {
		t.Fatalf("0.001 克是合法的正数用量: %v", err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Materials) != 1 {
		t.Fatalf("应按物料逐项核对: %+v", b.Materials)
	}
	m := b.Materials[0]
	if m.RequiredGrams != "0.003" || m.ActualGrams != "0" || m.DifferenceGrams != "-0.003" {
		t.Fatalf("无投料时累计实投为零、差额为负均属正常，得到 %+v", m)
	}

	// 重新打开（触发完整校验）后数据仍可用。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("含 0.001 克用量的台账应能正常打开: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetRecipe("R1", "v1"); err != nil {
		t.Fatalf("重新打开后配方查询应成功: %v", err)
	}
}
