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

// 构造两条具有相同配方编号与版本号的配方记录；
// identical 为 true 时两条内容（名称、物料、每份克数）完全一致。
func recipeVersionDupRecords(identical bool) []*recipeRecord {
	first := &recipeRecord{RecipeNo: "R2", Version: "v1", Name: "配方二",
		Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}}}
	var second *recipeRecord
	if identical {
		second = &recipeRecord{RecipeNo: "R2", Version: "v1", Name: "配方二",
			Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}}}
	} else {
		second = &recipeRecord{RecipeNo: "R2", Version: "v1", Name: "配方二改名",
			Materials: []materialRecord{{MaterialNo: "M2", GramsMilli: 200000}}}
	}
	return []*recipeRecord{first, second}
}

// Open 时只要存在两条配方编号与版本号完全相同的配方记录就必须返回
// ErrCorruptData：内容不同、内容完全一致、重复版本未被任何批次选用，
// 三种情形都拒绝；错误信息需包含重复的配方编号与版本号；
// 不返回可用的台账对象，也不改写原文件；不得挑第一条或最后一条放行。
func TestOpenRejectsDuplicateRecipeVersion(t *testing.T) {
	cases := []struct {
		name      string
		identical bool
		withUse   bool // 是否有批次绑定重复版本
	}{
		{"名称物料不同且有批次绑定", false, true},
		{"两条记录内容完全一致", true, true},
		{"重复版本未被任何批次选用", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := &persistedState{
				Version: stateVersion,
				Recipes: append([]*recipeRecord{recipeR1v1()}, recipeVersionDupRecords(tc.identical)...),
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
				t.Fatalf("重复配方版本应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"R2", "v1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			// 原台账内容保留：不得挑一条、合并物料或自行删除一条后另存。
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

// 台账里其他配方与批次都完整也不能绕过：只要一组编号+版本号出现两条，
// Open 整体失败，即使重复版本本身未被任何批次引用。
func TestOpenDuplicateRecipeVersionAmongCompleteRecords(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: append(
			[]*recipeRecord{recipeR1v1()},
			recipeVersionDupRecords(false)...,
		),
		Batches: []*batchRecord{
			{BatchNo: "B-ok", RecipeNo: "R1", RecipeVersion: "v1", PlannedPortions: 1, Status: StatusClosed},
		},
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("存在完整配方与批次也应整体拒绝，得到 %v", err)
	}
}

// 台账打开后本地文件被改坏（多出一条相同编号+版本号的配方记录）：
// 下一次查询或写入都必须返回 ErrCorruptData，即使查询的是另一个正常版本、
// 修改的是另一个正常批次；不得凭此前读取过的内容返回旧结果；
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
		{MaterialNo: "M1", Grams: "5"},
	}); err != nil {
		t.Fatal(err)
	}
	// B1 绑定正常版本 R1/v1；重复将发生在未被批次引用的 R2/v1 上。
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

	// 保存完好时的文件内容，随后追加一条 R2/v1 记录（编号+版本号重复）。
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	broken.Recipes = append(broken.Recipes, &recipeRecord{
		RecipeNo: "R2", Version: "v1", Name: "配方二改名",
		Materials: []materialRecord{{MaterialNo: "M9", GramsMilli: 7000}},
	})
	badBytes := writeStateFile(t, dir, &broken)

	// 查询：重复版本、正常版本，以及绑定正常版本的批次，全部失败；
	// 不能凭此前读取过的内容返回旧结果。
	if _, err := s.GetRecipe("R2", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("文件损坏后查询重复版本应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使查询另一个正常版本也必须拒绝，得到 %v", err)
	}
	if _, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("即使查询绑定正常版本的批次也必须拒绝，得到 %v", err)
	}

	// 写入：修改正常批次、登记新配方都必须被拒绝（不崩溃、不放行）。
	if _, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭批次应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.UpdateDraftBatch("upd-x", "B1", "", "", 9); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整批次应返回 ErrCorruptData，得到 %v", err)
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

	// 删除重复记录恢复原状后，原有记录完整可用，批次仍绑定原版本。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询应成功: %v", err)
	}
	if b1.RecipeNo != "R1" || b1.RecipeVersion != "v1" ||
		len(b1.Materials) != 1 || b1.Materials[0].RequiredGrams != "200" ||
		b1.Materials[0].ActualGrams != "50" {
		t.Fatalf("数量核对仍应按绑定版本计算: %+v", b1.Materials)
	}
	// 损坏前已成功的请求仍可幂等重放。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "50", fixedTime, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	// 被拒绝过的请求编号仍可用于合法提交。
	f, err := s.AddFeeding("feed-rejected", "B1", "M1", "1", time.Now(), "李四")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 同一配方的不同版本、不同配方使用相同版本号仍是合法组合，正常登记、
// 查询并供批次选择；数量核对按各自绑定版本的物料与每份克数计算。
func TestOpenAllowsDifferentVersionsAndSharedVersionNumber(t *testing.T) {
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
		if b.Materials[0].RequiredGrams != want.required {
			t.Fatalf("批次 %q 应投量应按绑定版本计算，得到 %+v", want.batchNo, b.Materials)
		}
	}

	// 重新打开仍应正常。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("合法台账重新打开应成功: %v", err)
	}
	defer s2.Close()
	r, err := s2.GetRecipe("R2", "v1")
	if err != nil || r.Name != "配方二" {
		t.Fatalf("不同配方共用版本号应精确区分，得到 %v %+v", err, r)
	}
}

// 通过正常登记入口再次提交已存在版本仍按普通登记冲突处理
// （ErrRecipeExists），不能与文件损坏的 ErrCorruptData 混用，
// 且失败不占用请求编号。
func TestRegisterRecipeExistingVersionStillRecipeExists(t *testing.T) {
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
	_, err = s.RegisterRecipe("r1-again", "R1", "v1", "配方一", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	})
	if !errors.Is(err, ErrRecipeExists) {
		t.Fatalf("重复登记应返回 ErrRecipeExists，得到 %v", err)
	}
	if errors.Is(err, ErrCorruptData) {
		t.Fatalf("普通登记冲突不应归为数据损坏，得到 %v", err)
	}
	// 被拒绝的请求编号未被占用，可用于合法提交。
	if _, err := s.RegisterRecipe("r1-again", "R9", "v9", "其他配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "1"},
	}); err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
}
