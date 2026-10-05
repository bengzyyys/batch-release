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

// Open 时任一已保存配方版本没有任何物料，都必须返回 ErrCorruptData：
// 物料列表为空数组、为 null、字段缺失属于同一种错误；即使名称、编号、
// 版本号齐全且台账能正常解析也不例外。错误信息需包含配方编号、版本号并
// 明确说明没有物料；不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsRecipeVersionWithoutMaterials(t *testing.T) {
	cases := []struct {
		name    string
		write   func(t *testing.T, dir string) []byte
		wantIDs []string // 错误信息必须包含的编号片段
	}{
		{
			name: "物料列表为空数组",
			write: func(t *testing.T, dir string) []byte {
				return writeStateFile(t, dir, &persistedState{
					Version: stateVersion,
					Recipes: []*recipeRecord{
						{RecipeNo: "R1", Version: "v1", Name: "配方一",
							Materials: []materialRecord{}},
					},
				})
			},
			wantIDs: []string{"R1", "v1"},
		},
		{
			name: "物料列表为 null",
			write: func(t *testing.T, dir string) []byte {
				return writeStateFile(t, dir, &persistedState{
					Version: stateVersion,
					Recipes: []*recipeRecord{
						{RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: nil},
					},
				})
			},
			wantIDs: []string{"R1", "v1"},
		},
		{
			name: "物料字段缺失",
			write: func(t *testing.T, dir string) []byte {
				// 直接写原始 JSON：省略 materials 字段，其余内容完全合法。
				raw := []byte(`{
  "version": 1,
  "recipes": [
    {"recipeNo": "R1", "version": "v1", "name": "配方一"}
  ],
  "batches": [],
  "requests": {}
}`)
				if err := os.WriteFile(filepath.Join(dir, stateFileName), raw, 0o644); err != nil {
					t.Fatalf("写入台账文件失败: %v", err)
				}
				return raw
			},
			wantIDs: []string{"R1", "v1"},
		},
		{
			name: "空物料版本被执行中批次采用且尚未投料",
			write: func(t *testing.T, dir string) []byte {
				return writeStateFile(t, dir, &persistedState{
					Version: stateVersion,
					Recipes: []*recipeRecord{
						{RecipeNo: "R1", Version: "v1", Name: "配方一",
							Materials: []materialRecord{}},
					},
					Batches: []*batchRecord{{
						BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
						PlannedPortions: 10, Status: StatusExecuting,
					}},
				})
			},
			wantIDs: []string{"R1", "v1"},
		},
		{
			name: "未被任何批次引用的空物料版本",
			write: func(t *testing.T, dir string) []byte {
				return writeStateFile(t, dir, &persistedState{
					Version: stateVersion,
					Recipes: []*recipeRecord{
						recipeR1v1(),
						{RecipeNo: "R2", Version: "v3", Name: "配方二",
							Materials: nil},
					},
					Batches: []*batchRecord{{
						BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
						PlannedPortions: 1, Status: StatusClosed,
					}},
				})
			},
			wantIDs: []string{"R2", "v3"},
		},
		{
			name: "同一配方编号下另有完整版本也不能顶替",
			write: func(t *testing.T, dir string) []byte {
				empty := &recipeRecord{RecipeNo: "R1", Version: "v2", Name: "配方二",
					Materials: []materialRecord{}}
				return writeStateFile(t, dir, &persistedState{
					Version: stateVersion,
					Recipes: []*recipeRecord{recipeR1v1(), empty},
					Batches: []*batchRecord{{
						BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
						PlannedPortions: 1, Status: StatusDraft,
					}},
				})
			},
			wantIDs: []string{"R1", "v2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			original := tc.write(t, dir)

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
			if !strings.Contains(msg, "没有物料") {
				t.Fatalf("错误信息应明确说明该版本没有物料，得到 %v", err)
			}
			for _, want := range tc.wantIDs {
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

// 份数为正、状态合法、尚未投料都不能挽救空物料版本：即使没有任何数量
// 差额，采用该版本的批次也得不到核对项，Open 必须整体失败。
func TestOpenRejectsEmptyMaterialsDespiteLegalBatchAndNoFeedings(t *testing.T) {
	for _, status := range []BatchStatus{StatusDraft, StatusExecuting, StatusClosed} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			writeStateFile(t, dir, &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{
					{RecipeNo: "R1", Version: "v1", Name: "配方一",
						Materials: []materialRecord{}},
				},
				Batches: []*batchRecord{{
					BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 7, Status: status,
				}},
			})
			if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("批次 %s、正份数、无投料不能绕过空物料校验，得到 %v", status, err)
			}
		})
	}
}

// 没有登记任何配方的空台账仍可正常使用；这与“存在一个没有物料的配方版本”
// 必须区分开。合法版本中的物料尚未投料时，批次查询仍列出应投量、零实投量
// 与相应差额。
func TestEmptyLedgerUsableButEmptyMaterialRecipeIsNot(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("没有任何配方的空台账应能正常打开: %v", err)
	}
	// 登记一个合法配方与批次，验证无投料时数量核对项仍完整。
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Materials) != 1 {
		t.Fatalf("合法版本无投料时仍应列出核对项，得到 %+v", b.Materials)
	}
	m := b.Materials[0]
	if m.RequiredGrams != "200" || m.ActualGrams != "0" || m.DifferenceGrams != "-200" {
		t.Fatalf("无投料时应投/实投/差额不正确，得到 %+v", m)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 同一目录若出现一个空物料版本，则不再是可接受的空台账。
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R9", Version: "v1", Name: "空配方", Materials: nil},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("含空物料配方版本的台账应拒绝读取，得到 %v", err)
	}
}

// 台账打开后保存内容才出现空物料版本：下一次配方查询、批次查询或写入
// 重新读取时都必须返回 ErrCorruptData；写入不得在损坏内容上登记新记录，
// 被拒绝的写入不改动文件、不占用请求编号；恢复物料后原记录继续可用。
func TestCorruptEmptyMaterialsAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	// 把已保存版本的物料列表清空（不经过台账接口，模拟外部破坏）。
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	broken.Recipes[0].Materials = nil
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：配方与批次都不得返回部分结果或沿用此前的正确物料。
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

	// 恢复物料后，原记录完整可用；被拒绝过的请求编号仍可合法使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("恢复后配方查询应成功: %v", err)
	}
	if len(r.Materials) != 1 || r.Materials[0].Grams != "100" {
		t.Fatalf("恢复后配方物料应为原值: %+v", r)
	}
	f, err := s.AddFeeding("f-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}
