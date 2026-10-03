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

// 构造一个物料编号重复的配方版本记录。
func recipeDup(recipeNo, version string, grams1, grams2 gramsMilli) *recipeRecord {
	return &recipeRecord{RecipeNo: recipeNo, Version: version, Name: "配方",
		Materials: []materialRecord{
			{MaterialNo: "M1", GramsMilli: grams1},
			{MaterialNo: "M1", GramsMilli: grams2},
		}}
}

// Open 时只要任一配方版本内同一物料编号出现多次就必须返回 ErrCorruptData：
// 两条克数不同、完全相同、该版本未被任何批次选用，三种情形都拒绝；
// 错误信息需包含配方编号、版本号与重复的物料编号；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsDuplicateMaterialInRecipeVersion(t *testing.T) {
	cases := []struct {
		name    string
		grams1  gramsMilli
		grams2  gramsMilli
		withUse bool // 是否有批次绑定该版本
	}{
		{"克数不同且有批次绑定", 100000, 200000, true},
		{"克数完全相同", 100000, 100000, true},
		{"版本未被任何批次选用", 100000, 200000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{
					recipeR1v1(),
					recipeDup("R2", "v1", tc.grams1, tc.grams2),
				},
			}
			if tc.withUse {
				st.Batches = []*batchRecord{{
					BatchNo: "B1", RecipeNo: "R2", RecipeVersion: "v1",
					PlannedPortions: 2, Status: StatusExecuting,
				}}
			}
			original := writeStateFile(t, dir, st)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("配方版本内物料重复应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"R2", "v1", "M1"} {
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

// 台账里其他配方与批次都完整也不能绕过：只要一个版本物料重复，Open 整体失败。
func TestOpenDuplicateMaterialAmongCompleteRecords(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			recipeR1v1(),
			recipeDup("R2", "v1", 100000, 200000),
		},
		Batches: []*batchRecord{
			{BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在完整配方与批次也应整体拒绝，得到 %v", err)
	}
}

// 台账打开后本地文件被改坏（配方版本内物料编号重复）：
// 下一次查询或写入都必须返回 ErrCorruptData，不得返回部分结果、
// 不得返回重复的数量核对项、不得保存新记录或改变已有批次；
// 被拒绝的写入不留业务记录、不占用请求编号；恢复后原记录继续可用。
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
	if _, err := s.RegisterRecipe("r2", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M2", Grams: "5"},
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

	// 保存完好时的文件内容，随后把 R2/v1 改成 M2 重复两次（B1 绑定的 R1/v1 仍完整）。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	for _, r := range broken.Recipes {
		if r.RecipeNo == "R2" && r.Version == "v1" {
			r.Materials = append(r.Materials, materialRecord{MaterialNo: "M2", GramsMilli: 9000})
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：绑定完整配方的批次与配方查询也都必须失败，不得返回部分结果。
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询配方应返回 ErrCorruptData，得到 %v", err)
	}

	// 写入：投料、状态变化、登记配方都必须被拒绝。
	if _, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r3", "R3", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
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
	if bytes.Contains(after, []byte("feed-rejected")) || bytes.Contains(after, []byte("R3")) {
		t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
	}

	// 恢复原始内容后，原有记录完整可用，被拒绝过的请求编号仍可合法使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if len(b1.Materials) != 1 || b1.Materials[0].MaterialNo != "M1" ||
		b1.Materials[0].RequiredGrams != "200" || b1.Materials[0].ActualGrams != "50" {
		t.Fatalf("数量核对应按原配方逐项列出: %+v", b1.Materials)
	}
	f, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 不同版本及不同配方各自使用同一物料编号是合法共用，Open 正常接受；
// 批次的应投量只按自己绑定版本的每份克数与计划份数计算。
func TestOpenAllowsSameMaterialAcrossVersionsAndRecipes(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一 v1", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r2", "R1", "v2", "配方一 v2", []MaterialInput{
		{MaterialNo: "M1", Grams: "200"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r3", "R2", "v1", "配方二", []MaterialInput{
		{MaterialNo: "M1", Grams: "50"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b3", "B3", "R2", "v1", 2); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		batchNo  string
		required string
	}{
		{"B1", "200"},
		{"B2", "400"},
		{"B3", "100"},
	} {
		b, err := s.GetBatch(want.batchNo)
		if err != nil {
			t.Fatalf("查询批次 %q 失败: %v", want.batchNo, err)
		}
		if len(b.Materials) != 1 || b.Materials[0].RequiredGrams != want.required ||
			b.Materials[0].ActualGrams != "0" || b.Materials[0].DifferenceGrams != "-"+want.required {
			t.Fatalf("批次 %q 应投量应按绑定版本计算，得到 %+v", want.batchNo, b.Materials)
		}
	}
}

// 登记配方时物料编号重复仍按非法输入处理（ErrInvalidInput），
// 不能与读取已保存错误记录的 ErrCorruptData 混用。
func TestRegisterRecipeDuplicateMaterialStillInvalidInput(t *testing.T) {
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
		t.Fatalf("登记重复物料应返回 ErrInvalidInput，得到 %v", err)
	}
	if errors.Is(err, ErrCorruptData) {
		t.Fatalf("登记时的重复物料不应归为数据损坏，得到 %v", err)
	}
}
