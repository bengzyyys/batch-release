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

// rawRecipeRecord 用原始 JSON 控制 materials 字段的写法：传 nil 且 omitempty
// 时字段缺失，传 RawMessage("null") 写成 null，传 RawMessage("[]") 写成空
// 数组，从而模拟“字段缺失/null/空数组”这些 Go 切片类型无法区分的损坏形态。
type rawRecipeRecord struct {
	RecipeNo  string          `json:"recipeNo"`
	Version   string          `json:"version"`
	Name      string          `json:"name"`
	Materials json.RawMessage `json:"materials,omitempty"`
}

type rawRecipeState struct {
	Version int               `json:"version"`
	Recipes []rawRecipeRecord `json:"recipes"`
	Batches []*batchRecord    `json:"batches"`
}

func writeRawRecipeState(t *testing.T, dir string, st *rawRecipeState) []byte {
	t.Helper()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("序列化台账失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatalf("写入台账文件失败: %v", err)
	}
	return data
}

// Open 时任一已保存配方版本没有物料，都必须返回 ErrCorruptData：
// 物料列表为空数组、为 null、字段缺失都属于同一种错误；即使名称、编号、
// 版本号齐全、台账内容能正常解析也不能放行。错误信息需包含问题配方的编号
// 与版本号，并明确说明它没有物料；不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsRecipeVersionWithoutMaterials(t *testing.T) {
	// 采用空物料版本的批次：份数为正、状态合法、尚未投料，也不能以
	// “没有数量差额”为由继续返回结果。
	usedBatches := []*batchRecord{
		{BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 5, Status: StatusDraft},
	}
	executingBatch := []*batchRecord{
		{BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 2, Status: StatusExecuting},
	}
	cases := []struct {
		name      string
		materials json.RawMessage
		batches   []*batchRecord
	}{
		{"空数组且被草稿批次采用", json.RawMessage(`[]`), usedBatches},
		{"null 且被草稿批次采用", json.RawMessage(`null`), usedBatches},
		{"字段缺失且被草稿批次采用", nil, usedBatches},
		{"空数组且被执行中批次采用（尚未投料）", json.RawMessage(`[]`), executingBatch},
		{"null 且未被任何批次引用", json.RawMessage(`null`), nil},
		{"字段缺失且未被任何批次引用", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := writeRawRecipeState(t, dir, &rawRecipeState{
				Version: stateVersion,
				Recipes: []rawRecipeRecord{{
					RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: tc.materials,
				}},
				Batches: tc.batches,
			})

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("没有物料的配方版本应返回 ErrCorruptData，得到 %v", err)
			}
			if errors.Is(err, ErrInvalidInput) {
				t.Fatalf("已保存数据损坏不应报成提交参数错误，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"R1", "v1", "没有物料"} {
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

	// 通过持久化结构构造时，nil 切片序列化成 null、空切片序列化成 []，
	// 两种形态也都必须拒绝。
	for _, mats := range [][]materialRecord{nil, {}} {
		dir := t.TempDir()
		writeStateFile(t, dir, &persistedState{
			Version: stateVersion,
			Recipes: []*recipeRecord{{
				RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: mats,
			}},
		})
		s, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("物料列表为 %v 应返回 ErrCorruptData，得到 %v", mats, err)
		}
		if s != nil {
			s.Close()
			t.Fatalf("损坏台账不应返回可用的 Store 对象")
		}
	}
}

// 这项规则针对所有已保存版本，不只检查当前批次使用的版本：尚未被任何批次
// 引用的空物料版本也必须拒绝；同一配方编号下另有完整版本不能代替问题版本；
// 台账中存在其他正常配方与批次也不能绕过。
func TestOpenEmptyMaterialsVersionFailsWholeLedger(t *testing.T) {
	t.Run("未被引用的空物料版本", func(t *testing.T) {
		dir := t.TempDir()
		original := writeRawRecipeState(t, dir, &rawRecipeState{
			Version: stateVersion,
			Recipes: []rawRecipeRecord{
				{RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: json.RawMessage(`[{"materialNo":"M1","gramsMilli":100000}]`)},
				{RecipeNo: "R2", Version: "v9", Name: "未使用配方", Materials: json.RawMessage(`[]`)},
			},
			Batches: []*batchRecord{
				{BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed},
			},
		})
		s, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("未被批次引用的空物料版本也应整体拒绝，得到 %v", err)
		}
		if s != nil {
			s.Close()
			t.Fatalf("损坏台账不应返回可用的 Store 对象")
		}
		msg := err.Error()
		for _, want := range []string{"R2", "v9", "没有物料"} {
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

	t.Run("同编号另有完整版本不能代替", func(t *testing.T) {
		dir := t.TempDir()
		// R1/v1 完整且批次正常使用；R1/v2 没有物料、没有批次引用。
		writeStateFile(t, dir, &persistedState{
			Version: stateVersion,
			Recipes: []*recipeRecord{
				{RecipeNo: "R1", Version: "v1", Name: "配方一",
					Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}}},
				{RecipeNo: "R1", Version: "v2", Name: "配方一修订", Materials: nil},
			},
			Batches: []*batchRecord{
				{BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 3, Status: StatusDraft},
			},
		})
		s, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("同编号的完整版本不能代替空物料版本，得到 %v", err)
		}
		if s != nil {
			s.Close()
			t.Fatalf("损坏台账不应返回可用的 Store 对象")
		}
		msg := err.Error()
		for _, want := range []string{"R1", "v2", "没有物料"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("错误信息应指出问题版本 R1/v2，得到 %v", err)
			}
		}
	})

	t.Run("其他正常配方与批次不能绕过", func(t *testing.T) {
		dir := t.TempDir()
		writeRawRecipeState(t, dir, &rawRecipeState{
			Version: stateVersion,
			Recipes: []rawRecipeRecord{
				{RecipeNo: "R1", Version: "v1", Name: "正常配方", Materials: json.RawMessage(`[{"materialNo":"M1","gramsMilli":100000}]`)},
				{RecipeNo: "R-bad", Version: "v1", Name: "问题配方", Materials: json.RawMessage(`null`)},
			},
			Batches: []*batchRecord{
				{BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 4, Status: StatusExecuting},
			},
		})
		if s, err := Open(dir); !errors.Is(err, ErrCorruptData) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("台账存在空物料版本时，即使只想查询正常记录也必须拒绝，得到 %v", err)
		} else if s != nil {
			s.Close()
			t.Fatalf("损坏台账不应返回可用的 Store 对象")
		}
	})
}

// 没有登记任何配方的空台账仍可正常使用，必须与“存在一个没有物料的配方
// 版本”区分开：没有台账文件、显式空配方列表都应得到可用空台账。
func TestEmptyLedgerStillUsable(t *testing.T) {
	t.Run("没有台账文件", func(t *testing.T) {
		s := openTestStore(t)
		if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("空台账查询配方应返回 ErrNotFound，得到 %v", err)
		}
	})

	t.Run("台账文件中没有任何配方记录", func(t *testing.T) {
		dir := t.TempDir()
		writeStateFile(t, dir, &persistedState{Version: stateVersion})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("没有配方记录的台账应正常打开: %v", err)
		}
		defer s.Close()
		if _, err := s.GetBatch("B1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("空台账查询批次应返回 ErrNotFound，得到 %v", err)
		}
		// 空台账上仍可正常登记配方。
		if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
			{MaterialNo: "M1", Grams: "100"},
		}); err != nil {
			t.Fatalf("空台账应可继续登记配方: %v", err)
		}
	})
}

// 台账打开后保存内容出现空物料版本：下一次查询或写入重新读取时都必须返回
// ErrCorruptData，不得返回没有核对项的批次结果，也不得沿用此前读到的配方；
// 被拒绝的读取不改文件，被拒绝的写入不留业务记录、不占用请求编号；恢复后
// 原记录继续可用，合法版本尚未投料时批次查询仍列出应投量、零实投量与差额。
func TestEmptyMaterialsCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	t.Run("正在使用的版本被清空物料", func(t *testing.T) {
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
		// 份数为正、草稿、尚无投料：没有这条校验时它会得到零个核对项。
		if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
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
		broken.Recipes[0].Materials = nil
		badBytes := writeStateFile(t, dir, &broken)

		// 查询：批次与配方都不得返回可继续使用的结果。
		if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("文件损坏后批次查询应返回 ErrCorruptData，得到 %v", err)
		}
		if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("文件损坏后配方查询应返回 ErrCorruptData，得到 %v", err)
		}

		// 写入：登记配方、创建批次都必须被拒绝。
		if _, err := s.RegisterRecipe("r-rejected", "R2", "v1", "新配方", []MaterialInput{
			{MaterialNo: "M1", Grams: "1"},
		}); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏台账上登记配方应返回 ErrCorruptData，得到 %v", err)
		}
		if _, err := s.CreateBatch("b-rejected", "B2", "R1", "v1", 1); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏台账上创建批次应返回 ErrCorruptData，得到 %v", err)
		}

		// 被拒绝的操作不留痕迹：文件内容不变，请求编号未被占用。
		after, err := os.ReadFile(filepath.Join(dir, stateFileName))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, badBytes) {
			t.Fatalf("被拒绝的写入不应改动台账文件")
		}
		if bytes.Contains(after, []byte("r-rejected")) || bytes.Contains(after, []byte("b-rejected")) {
			t.Fatalf("被拒绝的写入不应留下请求结果或业务记录")
		}

		// 恢复后原记录完整可用；批次尚未投料时仍逐项列出应投量、零实投量
		// 与相应差额；被拒绝过的请求编号仍可合法使用。
		if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := s.GetBatch("B1")
		if err != nil {
			t.Fatalf("恢复后批次查询应成功: %v", err)
		}
		if len(b.Materials) != 1 {
			t.Fatalf("恢复后应按物料逐项核对，得到 %d 项", len(b.Materials))
		}
		m := b.Materials[0]
		if m.MaterialNo != "M1" || m.RequiredGrams != "1000" ||
			m.ActualGrams != "0" || m.DifferenceGrams != "-1000" {
			t.Fatalf("未投料时应列出应投量、零实投量与差额，得到 %+v", m)
		}
		if _, err := s.RegisterRecipe("r-rejected", "R2", "v1", "新配方", []MaterialInput{
			{MaterialNo: "M1", Grams: "1"},
		}); err != nil {
			t.Fatalf("被拒绝过的请求编号应仍可合法使用: %v", err)
		}
	})

	t.Run("新增未被引用的空物料版本", func(t *testing.T) {
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
		fixedTime := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
		if _, err := s.AddFeeding("f1", "B1", "M1", "5", fixedTime, "张三"); err != nil {
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
		// 外部改坏：凭空多出一个没有物料、也没有任何批次引用的版本。
		broken.Recipes = append(broken.Recipes, &recipeRecord{
			RecipeNo: "R9", Version: "v1", Name: "问题配方", Materials: nil,
		})
		badBytes := writeStateFile(t, dir, &broken)

		// 即使查询的是完全正常的批次与配方，整份台账也不能读取。
		if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("存在未引用的空物料版本时查询正常批次也应失败，得到 %v", err)
		}
		if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("存在未引用的空物料版本时查询正常配方也应失败，得到 %v", err)
		}
		if _, err := s.AddFeeding("f-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
		}
		after, err := os.ReadFile(filepath.Join(dir, stateFileName))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, badBytes) {
			t.Fatalf("被拒绝的写入不应改动台账文件")
		}
		if bytes.Contains(after, []byte("f-rejected")) {
			t.Fatalf("被拒绝的写入不应留下请求记录")
		}

		// 恢复后原投料完整可用，被拒绝过的投料编号仍可合法使用。
		if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := s.AddFeeding("f-rejected", "B1", "M1", "1", time.Now(), "李四")
		if err != nil {
			t.Fatalf("被拒绝过的请求编号应仍可合法使用: %v", err)
		}
		if f.Seq != 2 || f.Grams != "1" {
			t.Fatalf("新投料结果不正确: %+v", f)
		}
	})
}
