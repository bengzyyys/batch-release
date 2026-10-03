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

// 本组回归测试锁定“已保存投料必须属于批次绑定的配方版本”这条读取期规则：
// 登记投料（AddFeeding）本就会拒绝配方之外的物料，这里保证读取已有台账时的
// 同一检查不会在后续修改中被绕过，同时保留批次状态机、数量核对与请求编号的
// 公开行为。

// 构造绑定 R1/v1 的批次 B1（执行中、2 份），配方与投料由调用方指定。
func writeMembershipState(t *testing.T, dir string, recipes []*recipeRecord, feedings []feedingRecord, status BatchStatus) []byte {
	t.Helper()
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: recipes,
		Batches: []*batchRecord{{
			BatchNo:         "B1",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: 2,
			Status:          status,
			Feedings:        feedings,
		}},
	})
}

// 合法的第一条投料（M1、正数、累计远低于上限），各用例共用。
func validFirstFeeding() feedingRecord {
	return feedingRecord{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: time.Now(), Registrar: "张三"}
}

// Open 时，已保存投料使用了绑定版本没有的物料编号必须返回 ErrCorruptData：
// 物料在台账里完全不存在、只出现在同编号的其他版本（R1/v2）、
// 或只出现在另一配方（R2/v1），都不能作为这条投料合法的依据。
// 问题投料克数为正数、累计数量不超限，仍应按“归属错误”拒绝，
// 而不是混同为数量非法；错误信息必须能指出批次编号、投料登记序号、
// 问题物料编号以及批次绑定的配方编号和版本号，且不改写原文件。
func TestOpenRejectsFeedingMaterialOutsideBoundVersion(t *testing.T) {
	r1v1 := &recipeRecord{RecipeNo: "R1", Version: "v1", Name: "配方一-v1",
		Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}}}
	r1v2 := &recipeRecord{RecipeNo: "R1", Version: "v2", Name: "配方一-v2",
		Materials: []materialRecord{{MaterialNo: "M2", GramsMilli: 7000}}}
	r2v1 := &recipeRecord{RecipeNo: "R2", Version: "v1", Name: "配方二",
		Materials: []materialRecord{{MaterialNo: "M3", GramsMilli: 3000}}}

	cases := []struct {
		name       string
		recipes    []*recipeRecord
		materialNo string
	}{
		{"物料在台账中完全不存在", []*recipeRecord{r1v1}, "M9"},
		{"物料只出现在同编号的其他版本", []*recipeRecord{r1v1, r1v2}, "M2"},
		{"物料只出现在另一配方", []*recipeRecord{r1v1, r2v1}, "M3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// 问题投料为 5 克正数，M1 累计仅 1 克，数量与上限都没有问题。
			feedings := []feedingRecord{
				validFirstFeeding(),
				{Seq: 2, MaterialNo: tc.materialNo, GramsMilli: 5000, Time: time.Now(), Registrar: "李四"},
			}
			original := writeMembershipState(t, dir, tc.recipes, feedings, StatusExecuting)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("配方之外的已保存投料应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"B1", "2", tc.materialNo, "R1", "v1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q（批次、登记序号、物料、配方编号、版本号），得到 %v", want, err)
				}
			}
			if !strings.Contains(msg, "不属于") {
				t.Fatalf("错误信息应指出物料不属于绑定版本，得到 %v", err)
			}
			if strings.Contains(msg, "上限") || strings.Contains(msg, "不是正数") {
				t.Fatalf("归属错误不应被误报为数量非法，得到 %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("拒绝打开不应改动台账文件：不得删除投料、补入物料或替换版本")
			}
		})
	}
}

// 已关闭批次的已保存投料同样受归属规则约束：不能因为不再追加投料就跳过检查。
func TestOpenRejectsForeignMaterialFeedingOnClosedBatch(t *testing.T) {
	dir := t.TempDir()
	r1v1 := &recipeRecord{RecipeNo: "R1", Version: "v1", Name: "配方一",
		Materials: []materialRecord{{MaterialNo: "M1", GramsMilli: 100000}}}
	original := writeMembershipState(t, dir, []*recipeRecord{r1v1}, []feedingRecord{
		validFirstFeeding(),
		{Seq: 2, MaterialNo: "M9", GramsMilli: 5000, Time: time.Now(), Registrar: "李四"},
	}, StatusClosed)

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("已关闭批次的配方外投料也应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"B1", "2", "M9", "R1", "v1"} {
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
}

// 台账正常打开以后，保存内容中的投料物料变得不匹配绑定版本：
// 下一次查询批次或追加投料必须返回 ErrCorruptData，不能继续使用之前读取的内容；
// 台账中另有完整批次时，查询或追加那个批次的投料同样不能绕过问题。
// 被拒绝的查询不能返回部分核对结果；被拒绝的写入不留业务变化、不占用请求编号，
// 原台账内容保持不变（不删除问题投料、不补入物料、不替换配方版本）。
func TestForeignMaterialCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// R1/v1 只有 M1；M2 只出现在 R1/v2，正适合作为“只在其他版本存在”的问题物料。
	if _, err := s.RegisterRecipe("r1", "R1", "v1", "配方一-v1", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRecipe("r2", "R1", "v2", "配方一-v2", []MaterialInput{
		{MaterialNo: "M1", Grams: "150"},
		{MaterialNo: "M2", Grams: "7"},
	}); err != nil {
		t.Fatal(err)
	}
	// B1 绑定 R1/v1（将被改坏）；B2 绑定 R1/v2 且始终完整。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-b1", "B1", "M1", "50", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-b2", "B2", "M2", "5", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}

	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 把 B1 的保存内容改坏：追加一条 M2 投料（正数、不超限），
	// 但 M2 不在 B1 绑定的 R1/v1 中；B2 仍完整。
	var broken persistedState
	if err := json.Unmarshal(good, &broken); err != nil {
		t.Fatal(err)
	}
	for _, b := range broken.Batches {
		if b.BatchNo == "B1" {
			b.Feedings = append(b.Feedings, feedingRecord{
				Seq: 2, MaterialNo: "M2", GramsMilli: 5000, Time: fixedTime, Registrar: "李四",
			})
		}
	}
	badBytes := writeStateFile(t, dir, &broken)

	// 查询损坏批次与完整批次都必须失败，且不能返回部分核对结果。
	for _, batchNo := range []string{"B1", "B2"} {
		view, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("文件改坏后查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if view != nil {
			t.Fatalf("被拒绝的查询 %q 不应返回部分核对结果，得到 %+v", batchNo, view)
		}
	}

	// 追加投料：对损坏批次本身、对完整批次 B2，都必须被拒绝。
	if _, err := s.AddFeeding("feed-rejected-b1", "B1", "M1", "1", time.Now(), "王五"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏批次上追加投料应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-rejected-b2", "B2", "M2", "1", time.Now(), "王五"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("不能借操作完整批次绕过读取失败，得到 %v", err)
	}

	// 被拒绝的写入不留痕迹：文件保持改坏后的原样，请求编号未被占用。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, badBytes) {
		t.Fatalf("被拒绝的操作不应改动台账文件：不得删除问题投料、补入物料或替换配方版本")
	}
	if bytes.Contains(after, []byte("feed-rejected")) {
		t.Fatalf("被拒绝的写入不应留下请求记录或占用请求编号")
	}

	// 恢复原文件后：B1 仍绑定 R1/v1，问题投料不存在、物料行仍以 v1 为准；
	// B2 原有投料保留；被拒绝过的请求编号仍可合法提交并拿到连续序号。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("恢复后查询 B1 应成功: %v", err)
	}
	if b1.RecipeNo != "R1" || b1.RecipeVersion != "v1" {
		t.Fatalf("恢复后绑定配方版本不应被替换，得到 %q/%q", b1.RecipeNo, b1.RecipeVersion)
	}
	if len(b1.Feedings) != 1 || b1.Feedings[0].MaterialNo != "M1" || b1.Feedings[0].Grams != "50" {
		t.Fatalf("恢复后 B1 应只剩原合法投料，得到 %+v", b1.Feedings)
	}
	if len(b1.Materials) != 1 || b1.Materials[0].MaterialNo != "M1" {
		t.Fatalf("数量核对仍应按绑定版本 v1 列物料，不得补入 M2，得到 %+v", b1.Materials)
	}
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("恢复后查询 B2 应成功: %v", err)
	}
	if len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
		t.Fatalf("完整批次的原有投料应保留: %+v", b2.Feedings)
	}
	// 损坏前已成功的请求仍可幂等重放。
	replay, err := s.AddFeeding("feed-b2", "B2", "M2", "5", fixedTime, "张三")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	if replay.Seq != 1 {
		t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
	}
	// 被拒绝过的写入没有占用编号，也没有占用 B2 的投料序号。
	f, err := s.AddFeeding("feed-rejected-b2", "B2", "M2", "1", time.Now(), "王五")
	if err != nil {
		t.Fatalf("被拒绝的请求编号应仍可合法使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料应取得下一个登记序号，得到 %+v", f)
	}
}

// 合法情形保留：同一物料可以同时出现在多个配方版本中，只要它存在于批次
// 真正绑定的版本就能正常读取；各版本每份用量不同时，应投量按绑定版本与
// 计划份数计算，累计实投与差额按该物料的实际记录计算。实投不足、超过应投量
// 或没有投料都属于数量核对结果，不能被归属检查误判为台账损坏。
func TestSharedMaterialReadsByBoundVersionAndQuantityCheck(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R1", Version: "v1", Name: "配方一-v1", Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000}, // 每份 100 克
				{MaterialNo: "M2", GramsMilli: 500},    // 每份 0.5 克
				{MaterialNo: "M3", GramsMilli: 10000},  // 每份 10 克，本批次无投料
			}},
			{RecipeNo: "R1", Version: "v2", Name: "配方一-v2", Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 150000}, // 同一物料，每份用量不同
				{MaterialNo: "M2", GramsMilli: 250},
			}},
		},
		Batches: []*batchRecord{
			{
				BatchNo: "B1", RecipeNo: "R1", RecipeVersion: "v1",
				PlannedPortions: 2, Status: StatusExecuting,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M1", GramsMilli: 250000, Time: time.Now(), Registrar: "张三"}, // 250 克：超投
					{Seq: 2, MaterialNo: "M2", GramsMilli: 250, Time: time.Now(), Registrar: "张三"},    // 0.25 克：欠投
				},
			},
			{
				BatchNo: "B2", RecipeNo: "R1", RecipeVersion: "v2",
				PlannedPortions: 2, Status: StatusExecuting,
				Feedings: []feedingRecord{
					{Seq: 1, MaterialNo: "M1", GramsMilli: 150000, Time: time.Now(), Registrar: "李四"}, // 150 克：欠投
				},
			},
		},
	})

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("共用物料存在于绑定版本时应正常打开: %v", err)
	}
	defer s.Close()

	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("绑定 v1 的批次应正常读取: %v", err)
	}
	got := map[string]MaterialRequirement{}
	for _, m := range b1.Materials {
		got[m.MaterialNo] = m
	}
	want := map[string][3]string{
		// 应投量按 v1 每份用量 × 2 份；实投/差额按实际记录。
		"M1": {"200", "250", "50"},   // 超投不抵消其他物料
		"M2": {"1", "0.25", "-0.75"}, // 欠投
		"M3": {"20", "0", "-20"},     // 无投料仍显示零实投
	}
	for no, w := range want {
		m, ok := got[no]
		if !ok {
			t.Fatalf("v1 的物料 %q 应出现在核对结果中", no)
		}
		if m.RequiredGrams != w[0] || m.ActualGrams != w[1] || m.DifferenceGrams != w[2] {
			t.Fatalf("物料 %q 数量核对不符：应投 %s 实投 %s 差额 %s，得到 %+v",
				no, w[0], w[1], w[2], m)
		}
	}
	if len(b1.Feedings) != 2 {
		t.Fatalf("实投记录应原样保留，得到 %+v", b1.Feedings)
	}

	// 同一物料 M1 在 v2 每份 150 克：B2 的应投量按 v2 计算，不沿用 v1。
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatalf("绑定 v2 的批次应正常读取: %v", err)
	}
	var m1 MaterialRequirement
	for _, m := range b2.Materials {
		if m.MaterialNo == "M1" {
			m1 = m
		}
	}
	if m1.RequiredGrams != "300" || m1.ActualGrams != "150" || m1.DifferenceGrams != "-150" {
		t.Fatalf("v2 批次应按绑定版本核对数量，得到 %+v", m1)
	}

	// 打开后台账仍可正常继续投料（公开行为保留）。
	f, err := s.AddFeeding("feed-more", "B1", "M3", "20", time.Now(), "张三")
	if err != nil {
		t.Fatalf("合法投料不应受归属检查影响: %v", err)
	}
	if f.Seq != 3 {
		t.Fatalf("新投料应按登记顺序取得序号 3，得到 %d", f.Seq)
	}
	b1, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range b1.Materials {
		if m.MaterialNo == "M3" && (m.ActualGrams != "20" || m.DifferenceGrams != "0") {
			t.Fatalf("补投后 M3 应实投 20、差额 0，得到 %+v", m)
		}
	}
}
