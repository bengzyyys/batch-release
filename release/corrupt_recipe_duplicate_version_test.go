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

// 再构造一条 R1/v1 配方记录，内容可与 recipeR1v1 不同或完全一致。
func recipeR1v1Alt(name string, grams gramsMilli) *recipeRecord {
	return &recipeRecord{RecipeNo: "R1", Version: "v1", Name: name,
		Materials: []materialRecord{{MaterialNo: "M9", GramsMilli: grams}}}
}

// Open 时只要同一配方编号与版本号出现第二条记录就必须返回 ErrCorruptData：
// 两条内容不同、完全一致、重复版本未被任何批次选用，三种情形都拒绝；
// 错误信息需包含重复的配方编号与版本号；
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsDuplicateRecipeVersion(t *testing.T) {
	cases := []struct {
		name    string
		second  *recipeRecord
		withUse bool // 是否有批次绑定该重复版本
	}{
		{"名称物料不同且有批次绑定", recipeR1v1Alt("另一个名称", 200000), true},
		{"两条记录完全一致", recipeR1v1(), true},
		{"重复版本未被任何批次选用", recipeR1v1Alt("另一个名称", 200000), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := &persistedState{
				Version: stateVersion,
				Recipes: []*recipeRecord{
					recipeR1v1(),
					tc.second,
				},
			}
			if tc.withUse {
				st.Batches = []*batchRecord{{
					BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
					PlannedPortions: 2, Status: StatusExecuting,
				}}
			}
			original := writeStateFile(t, dir, st)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("重复配方版本应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"R1", "v1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			// 原台账内容保留：不得挑第一条或最后一条、合并物料或自行删除一条。
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

// 台账里其他配方与批次都完整也不能绕过：只要一组编号+版本号重复，Open 整体失败。
func TestOpenDuplicateRecipeVersionAmongCompleteRecords(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			recipeR1v1(),
			{RecipeNo: "R2", Version: "v1", Name: "配方二",
				Materials: []materialRecord{{MaterialNo: "M2", GramsMilli: 5000}}},
			recipeR1v1Alt("另一个名称", 200000),
		},
		Batches: []*batchRecord{
			{BatchNo: "B-ok", RecipeNo: "R2", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在完整配方与批次也应整体拒绝，得到 %v", err)
	}
}

// 不同版本（同编号不同版本号）与不同配方使用相同版本号都是合法的，
// Open 正常接受，批次继续绑定各自选择的版本，数量核对按该版本计算。
func TestOpenAllowsDifferentVersionsAndSameVersionNo(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.RegisterRecipe("r1-v1", "R1", "v1", "配方一 v1", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r1-v2", "R1", "v2", "配方一 v2", []MaterialInput{
		{MaterialNo: "M1", Grams: "200"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r2-v1", "R2", "v1", "配方二 v1", []MaterialInput{
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
		if b.Materials[0].RequiredGrams != want.required {
			t.Fatalf("批次 %q 应投量应按绑定版本计算，得到 %+v", want.batchNo, b.Materials)
		}
	}
}

// 台账打开后本地文件被改坏（同一编号与版本号出现两条记录）：
// 下一次查询或写入都必须返回 ErrCorruptData，即使只查询另一个正常版本、
// 只修改另一个正常批次也不能绕过；不得凭此前读取的内容返回旧结果。
// 被拒绝的写入不留业务记录、不占用请求编号；恢复后原记录继续可用。
func TestDuplicateRecipeVersionAfterOpenDetectedOnNextAccess(t *testing.T) {
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
	// B1 绑定出现重复的 R1/v1；B2 绑定完全正常的 R2/v1。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R2", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-b2", "B2", "M2", "5", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	// 保存完好时的文件内容，随后再塞入第二条 R1/v1（R2/v1 与 B2 仍完整）。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	broken.Recipes = append(broken.Recipes, recipeR1v1Alt("另一个名称", 200000))
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：重复版本涉及的批次、另一个正常批次、正常版本的配方查询全部失败；
	// 不能凭此前读到的内容返回旧结果。
	for _, batchNo := range []string{"B1", "B2"} {
		if _, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("文件损坏后查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
	}
	if _, err := s.GetRecipe("R2", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询正常版本也应返回 ErrCorruptData，得到 %v", err)
	}

	// 写入：修改另一个正常批次、投料、登记新配方都必须被拒绝。
	if _, err := s.CloseBatch("close-b2", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("不能借操作正常批次绕过读取失败，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-rejected", "B2", "M2", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.RegisterRecipe("r3", "R3", "v1", "新配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上登记配方也应失败，得到 %v", err)
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
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if b2.Status != StatusExecuting || len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
		t.Fatalf("原有投料记录应保留: %+v", b2)
	}
	// 损坏前已成功的请求仍可幂等重放。
	replay, err := s.AddFeeding("feed-b2", "B2", "M2", "5", fixedTime, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	// 被拒绝过的请求编号仍可用于合法提交。
	f, err := s.AddFeeding("feed-rejected", "B2", "M2", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 通过正常登记入口再次提交已存在版本仍走 ErrRecipeExists，
// 不能把普通登记冲突混成文件损坏。
func TestRegisterExistingVersionStillRecipeExists(t *testing.T) {
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
	// 内容不同。
	_, err = s.RegisterRecipe("r2", "R1", "v1", "另一个名称", []MaterialInput{
		{MaterialNo: "M1", Grams: "200"},
	})
	if !errors.Is(err, ErrRecipeExists) {
		t.Fatalf("重复登记应返回 ErrRecipeExists，得到 %v", err)
	}
	if errors.Is(err, ErrCorruptData) {
		t.Fatalf("正常登记冲突不应归为数据损坏，得到 %v", err)
	}
	// 内容完全一致也仍是登记冲突。
	_, err = s.RegisterRecipe("r3", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	})
	if !errors.Is(err, ErrRecipeExists) {
		t.Fatalf("相同内容重复登记应返回 ErrRecipeExists，得到 %v", err)
	}
}
