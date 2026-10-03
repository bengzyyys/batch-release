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

// Open 时任一配方版本的物料列表内同一物料编号出现两次，都必须返回
// ErrCorruptData：不论两条记录克数是否相同、该版本是否被批次使用；
// 错误信息需包含配方编号、版本号与重复的物料编号；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsDuplicateMaterialInRecipeVersion(t *testing.T) {
	cases := []struct {
		name    string
		recipes []*recipeRecord
		batches []*batchRecord
		want    []string // 错误信息必须包含的片段
	}{
		{
			name: "被批次使用的版本同物料两种克数",
			recipes: []*recipeRecord{
				{RecipeNo: "R1", Version: "v1", Name: "配方一",
					Materials: []materialRecord{
						{MaterialNo: "M1", GramsMilli: 100000},
						{MaterialNo: "M1", GramsMilli: 200000},
					}},
			},
			batches: []*batchRecord{
				{BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 2, Status: StatusExecuting},
				{BatchNo: "B2", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 2, Status: StatusDraft},
			},
			want: []string{"R1", "v1", "M1"},
		},
		{
			name: "重复的两条克数完全相同",
			recipes: []*recipeRecord{
				{RecipeNo: "R1", Version: "v1", Name: "配方一",
					Materials: []materialRecord{
						{MaterialNo: "M1", GramsMilli: 100000},
						{MaterialNo: "M2", GramsMilli: 50000},
						{MaterialNo: "M1", GramsMilli: 100000},
					}},
			},
			batches: []*batchRecord{{
				BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusClosed,
			}},
			want: []string{"R1", "v1", "M1"},
		},
		{
			name: "未被任何批次使用的版本含重复物料",
			recipes: []*recipeRecord{
				recipeR1v1(),
				{RecipeNo: "R2", Version: "v3", Name: "配方二",
					Materials: []materialRecord{
						{MaterialNo: "M9", GramsMilli: 1000},
						{MaterialNo: "M9", GramsMilli: 2000},
					}},
			},
			batches: []*batchRecord{{
				BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 1, Status: StatusExecuting,
			}},
			want: []string{"R2", "v3", "M9"},
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
				t.Fatalf("配方版本内物料重复应返回 ErrCorruptData，得到 %v", err)
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
			// 原台账内容保留：不得合并重复项、相加克数或丢弃其中一条。
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

// 同一物料编号出现在不同版本或不同配方中是合法共用，Open 正常接受；
// 批次的应投量只按自己绑定版本的每份克数与计划份数计算。
func TestOpenAllowsSameMaterialAcrossVersionsAndRecipes(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R1", Version: "v1", Name: "配方一",
				Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}}},
			{RecipeNo: "R1", Version: "v2", Name: "配方一",
				Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 200000}}},
			{RecipeNo: "R2", Version: "v1", Name: "配方二",
				Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 300000}}},
		},
		Batches: []*batchRecord{
			{BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 2, Status: StatusExecuting},
			{BatchNo: "B2", RecipeNo: "R1", RecipeVersion: "v2",
				PlannedPortions: 2, Status: StatusExecuting},
			{BatchNo: "B3", RecipeNo: "R2", RecipeVersion: "v1",
				PlannedPortions: 2, Status: StatusDraft},
		},
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("跨版本、跨配方共用物料编号应被接受: %v", err)
	}
	defer s.Close()
	for _, want := range []struct {
		batchNo  string
		required string
	}{
		{"B1", "200"},
		{"B2", "400"},
		{"B3", "600"},
	} {
		b, err := s.GetBatch(want.batchNo)
		if err != nil {
			t.Fatalf("查询批次 %q 失败: %v", want.batchNo, err)
		}
		if len(b.Materials) != 1 || b.Materials[0].MaterialNo != "M1" ||
			b.Materials[0].RequiredGrams != want.required ||
			b.Materials[0].ActualGrams != "0" {
			t.Fatalf("批次 %q 应投量应按绑定版本计算为 %s，得到 %+v",
				want.batchNo, want.required, b.Materials)
		}
	}
}

// 台账打开后本地文件被改坏（配方版本内物料编号重复）：
// 下一次查询或写入都必须返回 ErrCorruptData，不得返回部分结果、
// 空台账或重复的数量核对项；被拒绝的写入不落盘、不占用请求编号；
// 恢复原记录后台账继续可用。
func TestDuplicateMaterialAfterOpenDetectedOnNextAccess(t *testing.T) {
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

	// 保存完好时的文件内容，随后把 R1/v1 改成 M1 出现两次（100 克与 200 克）。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	broken.Recipes[0].Materials = append(broken.Recipes[0].Materials,
		materialRecord{MaterialNo: "M1", GramsMilli: 200000})
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：不能返回部分结果或重复的数量核对项。
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 写入：不能保存新记录或改变已有批次。
	if _, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M2", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的写入不留痕迹：文件内容不变，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的写入不应改动台账文件")
	}
	if bytes.Contains(after, []byte("feed-rejected")) || bytes.Contains(after, []byte("R2")) {
		t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
	}

	// 恢复原记录后，台账继续可用；被拒绝过的请求编号仍可合法提交。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if len(b.Materials) != 1 || b.Materials[0].RequiredGrams != "200" {
		t.Fatalf("恢复后应投量应只按 100 克 × 2 份计算，得到 %+v", b.Materials)
	}
	f, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "张三")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 1 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 正常登记配方时物料编号重复仍按非法输入处理（ErrInvalidInput），
// 与读取已保存错误记录的 ErrCorruptData 不混用。
func TestRegisterRecipeDuplicateMaterialStaysInvalidInput(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M1", Grams: "200"},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("登记时物料重复应返回 ErrInvalidInput，得到 %v", err)
	}
	if errors.Is(err, ErrCorruptData) {
		t.Fatalf("登记时的非法输入不应误报为数据损坏，得到 %v", err)
	}
}
