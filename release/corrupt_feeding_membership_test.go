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

// 本文件是“已保存投料必须属于批次绑定的配方版本”的回归保障：
// 登记投料时拒绝配方之外的物料属于写入侧规则，这里锁定的是读取侧——
// 打开台账及之后的每次查询/写入重载，都必须重新执行同一归属检查，
// 不允许以后修改时在读取路径上绕过，同时保留批次状态与数量核对的公开行为。

// 构造 R1/v1、R1/v2、R2/v1 三个配方版本，以及一个绑定 R1/v1 的批次。
// 批次内第 1 条投料合法（M1 100 克），第 2 条使用 problemMaterial（50 克，
// 正数且累计远低于上限）。三种 problemMaterial 分别模拟：
//   - M9：台账中完全不存在；
//   - M2：只出现在同编号的另一版本 R1/v2；
//   - M3：只出现在另一配方 R2/v1。
func writeMembershipFeedingState(t *testing.T, dir, problemMaterial string, status BatchStatus) []byte {
	t.Helper()
	return writeStateFile(t, dir, &persistedState{
		Version: stateVersion,
		Recipes: []*recipeRecord{
			{RecipeNo: "R1", Version: "v1", Name: "配方一", Materials: []materialRecord{
				{MaterialNo: "M1", GramsMilli: 100000},
			}},
			{RecipeNo: "R1", Version: "v2", Name: "配方一改版", Materials: []materialRecord{
				{MaterialNo: "M2", GramsMilli: 200000},
			}},
			{RecipeNo: "R2", Version: "v1", Name: "配方二", Materials: []materialRecord{
				{MaterialNo: "M3", GramsMilli: 300000},
			}},
		},
		Batches: []*batchRecord{{
			BatchNo:         "B-bad",
			RecipeNo:        "R1",
			RecipeVersion:   "v1",
			PlannedPortions: 2,
			Status:          status,
			Feedings: []feedingRecord{
				{Seq: 1, MaterialNo: "M1", GramsMilli: 100000, Time: time.Now(), Registrar: "张三"},
				{Seq: 2, MaterialNo: problemMaterial, GramsMilli: 50000, Time: time.Now(), Registrar: "李四"},
			},
		}},
	})
}

// Open 读取已有台账时，已保存投料的物料不属于批次绑定版本必须返回
// ErrCorruptData：物料完全不存在、只出现在同编号其他版本、只出现在另一
// 配方中都不能作为合法依据；问题投料克数为正、累计不超限也不能放行。
// 错误信息必须指出批次编号、登记序号、问题物料编号以及绑定的配方编号
// 与版本号；不得返回可用的 Store，也不得改写原文件。
func TestOpenRejectsFeedingMaterialOutsideBoundVersion(t *testing.T) {
	materialCases := []struct {
		name string
		mat  string
	}{
		{"物料在台账中完全不存在", "M9"},
		{"物料只出现在同编号另一版本 R1/v2", "M2"},
		{"物料只出现在另一配方 R2/v1", "M3"},
	}
	// 已关闭批次不再追加投料，但其历史投料同样受归属规则约束，不能跳过。
	for _, status := range []BatchStatus{StatusExecuting, StatusClosed} {
		for _, tc := range materialCases {
			t.Run(string(status)+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				original := writeMembershipFeedingState(t, dir, tc.mat, status)

				s, err := Open(dir)
				if !errors.Is(err, ErrCorruptData) {
					t.Fatalf("投料物料不属于绑定版本应返回 ErrCorruptData，得到 %v", err)
				}
				if s != nil {
					s.Close()
					t.Fatalf("损坏台账不应返回可继续使用的 Store 对象")
				}
				msg := err.Error()
				for _, want := range []string{"B-bad", "第 2 条", tc.mat, "R1", "v1", "不属于"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
					}
				}
				// 读取失败不得擅自删除问题投料、补入物料或替换配方版本。
				got, readErr := os.ReadFile(filepath.Join(dir, stateFileName))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("拒绝打开不应改动台账文件")
				}
			})
		}
	}
}

// 唯一合法的修复方向是让物料进入批次真正绑定的版本：同编号另一版本
// 或另一配方里存在该物料不算数；手工把物料补入绑定版本 R1/v1 后，
// 原投料（含序号、克数、登记人）完整保留，数量核对按补入后的版本计算。
func TestOpenMembershipFailureRecoveredOnlyViaBoundVersion(t *testing.T) {
	dir := t.TempDir()
	// M2 只出现在 R1/v2：第一次打开必须拒绝，且文件不被自动修复。
	original := writeMembershipFeedingState(t, dir, "M2", StatusExecuting)
	if s, err := Open(dir); !errors.Is(err, ErrCorruptData) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("M2 只在 R1/v2 时应拒绝，得到 %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}

	// 手工把 M2 补入批次真正绑定的 R1/v1（R1/v2 保持不变），重新打开应成功。
	var repaired persistedState
	if err := json.Unmarshal(original, &repaired); err != nil {
		t.Fatal(err)
	}
	for _, r := range repaired.Recipes {
		if r.RecipeNo == "R1" && r.Version == "v1" {
			r.Materials = append(r.Materials, materialRecord{MaterialNo: "M2", GramsMilli: 25000})
		}
	}
	writeStateFile(t, dir, &repaired)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("物料进入绑定版本后应能打开: %v", err)
	}
	defer s.Close()
	view, err := s.GetBatch("B-bad")
	if err != nil {
		t.Fatal(err)
	}
	if view.RecipeNo != "R1" || view.RecipeVersion != "v1" || view.PlannedPortions != 2 {
		t.Fatalf("批次绑定信息应保持不变: %+v", view)
	}
	if len(view.Feedings) != 2 || view.Feedings[1].Seq != 2 ||
		view.Feedings[1].MaterialNo != "M2" || view.Feedings[1].Grams != "50" {
		t.Fatalf("原有两条投料应完整保留: %+v", view.Feedings)
	}
	mats := materialsMap(view)
	if len(mats) != 2 {
		t.Fatalf("应按补入后的 R1/v1 列出两种物料，得到 %d 项", len(mats))
	}
	// M1：100 × 2 = 200，实投 100；M2：25 × 2 = 50，实投 50（原问题投料）。
	checkRequirement(t, mats, "M1", "200", "100", "-100")
	checkRequirement(t, mats, "M2", "50", "50", "0")
}

// 台账正常打开后，保存内容中的投料物料变成不属于绑定版本：下一次查询
// 批次或追加投料必须返回 ErrCorruptData，不能沿用此前读到的内容；
// 台账中另有完整批次时，查询、追加、关闭那个批次同样不能绕过问题；
// 被拒绝的查询不返回部分核对结果，被拒绝的写入不留业务变化、不占用
// 请求编号，原台账内容保持不变；恢复后原有记录与请求行为完整可用。
func TestMembershipCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	cases := []struct {
		name string
		mat  string
	}{
		{"物料完全不存在", "M9"},
		{"物料只在 R1/v2", "M2"},
		{"物料只在另一配方", "M3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			if _, err := s.RegisterRecipe("r1-v1", "R1", "v1", "配方一", []MaterialInput{
				{MaterialNo: "M1", Grams: "100"},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RegisterRecipe("r1-v2", "R1", "v2", "配方一改版", []MaterialInput{
				{MaterialNo: "M2", Grams: "200"},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RegisterRecipe("r2-v1", "R2", "v1", "配方二", []MaterialInput{
				{MaterialNo: "M3", Grams: "300"},
			}); err != nil {
				t.Fatal(err)
			}
			// B1：随后被改坏的批次；B2：始终完整的批次（同绑 R1/v1）。
			if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartBatch("s1", "B1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartBatch("s2", "B2"); err != nil {
				t.Fatal(err)
			}
			fixedTime := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
			if _, err := s.AddFeeding("f-b1", "B1", "M1", "100", fixedTime, "张三"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "李四"); err != nil {
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
				if b.BatchNo == "B1" {
					// 50 克、正数、累计不超限；唯一问题是物料不属于 R1/v1。
					b.Feedings = append(b.Feedings, feedingRecord{
						Seq: 2, MaterialNo: tc.mat, GramsMilli: 50000,
						Time: fixedTime, Registrar: "王五",
					})
				}
			}
			badBytes := writeStateFile(t, dir, &broken)

			// 查询损坏批次：失败且不返回部分核对结果。
			if v, err := s.GetBatch("B1"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("改坏后查询 B1 应返回 ErrCorruptData，得到 %v", err)
			} else if v != nil {
				t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
			}
			// 查询完整批次同样不能绕过，也不能返回部分结果。
			if v, err := s.GetBatch("B2"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("查询完整批次 B2 也应失败，得到 %v", err)
			} else if v != nil {
				t.Fatalf("整份台账损坏时不应返回 B2 的部分视图: %+v", v)
			}
			// 配方查询同样重载整份台账，不能继续使用此前读到的内容。
			if _, err := s.GetRecipe("R1", "v1"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("改坏后查询配方也应返回 ErrCorruptData，得到 %v", err)
			}
			// 对完整批次追加投料、关闭批次都不能绕过读取检查。
			if _, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "赵六"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上对完整批次投料应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.AddFeeding("feed-bad-batch", "B1", "M1", "1", time.Now(), "赵六"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上对损坏批次投料应返回 ErrCorruptData，得到 %v", err)
			}
			if _, err := s.CloseBatch("close-rejected", "B2"); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("损坏台账上关闭完整批次也应返回 ErrCorruptData，得到 %v", err)
			}

			// 被拒绝的写入不留业务变化：文件内容不变，请求编号未被占用。
			after, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, badBytes) {
				t.Fatalf("被拒绝的写入不应改动台账文件")
			}
			for _, marker := range []string{"feed-rejected", "feed-bad-batch", "close-rejected"} {
				if bytes.Contains(after, []byte(marker)) {
					t.Fatalf("被拒绝的写入不应留下请求记录 %q", marker)
				}
			}

			// 恢复原内容后：问题投料从未被系统接受，两条合法投料完整，
			// 完整批次状态不变（关闭被拒绝），原成功请求仍可幂等重放，
			// 被拒绝过的请求编号可以正常使用并取得连续序号。
			if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
				t.Fatal(err)
			}
			b1, err := s.GetBatch("B1")
			if err != nil {
				t.Fatalf("恢复后查询 B1 应成功: %v", err)
			}
			if len(b1.Feedings) != 1 || b1.Feedings[0].MaterialNo != "M1" || b1.Feedings[0].Grams != "100" {
				t.Fatalf("B1 应只保留原合法投料: %+v", b1.Feedings)
			}
			b2, err := s.GetBatch("B2")
			if err != nil {
				t.Fatalf("恢复后查询 B2 应成功: %v", err)
			}
			if b2.Status != StatusExecuting {
				t.Fatalf("被拒绝的关闭不应改变 B2 状态，得到 %s", b2.Status)
			}
			if len(b2.Feedings) != 1 || b2.Feedings[0].Grams != "5" {
				t.Fatalf("B2 原有投料应保留: %+v", b2.Feedings)
			}
			replay, err := s.AddFeeding("f-b2", "B2", "M1", "5", fixedTime, "李四")
			if err != nil {
				t.Fatalf("恢复后重放原成功请求失败: %v", err)
			}
			if replay.Seq != 1 {
				t.Fatalf("重放应返回第一次成功的结果，得到 seq=%d", replay.Seq)
			}
			f, err := s.AddFeeding("feed-rejected", "B2", "M1", "1", time.Now(), "赵六")
			if err != nil {
				t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
			}
			if f.Seq != 2 || f.Grams != "1" {
				t.Fatalf("新投料结果不正确: %+v", f)
			}
		})
	}
}

// 合法情形保留：同一物料可以出现在多个配方版本中，只要存在于批次真正
// 绑定的版本就能正常读取；各版本每份用量不同时，应投量按绑定版本与
// 计划份数计算，累计实投与差额按该物料的实际记录计算。实投不足或超过
// 应投量只是数量核对结果，不能被归属检查误判为损坏；没有投料的配方
// 物料仍显示零实投。已关闭批次的合法投料在重新打开后同样完整可读。
func TestSameMaterialAcrossVersionsKeepsReconciliationBehavior(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// M1 同时存在于两个版本且每份用量不同（100 vs 250）；
	// M2、M0 只在 v1，M3 只在 v2。
	if _, err := s1.RegisterRecipe("r1-v1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M0", Grams: "10"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("r1-v2", "R1", "v2", "配方改版", []MaterialInput{
		{MaterialNo: "M1", Grams: "250"},
		{MaterialNo: "M3", Grams: "2"},
	}); err != nil {
		t.Fatal(err)
	}

	// B1 绑定 v1、3 份：M1 实投 100（应投 300，欠投），M2、M0 不投料。
	if _, err := s1.CreateBatch("b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.AddFeeding("f-b1", "B1", "M1", "100", time.Now(), "张三"); err != nil {
		t.Fatal(err)
	}
	// B2 绑定 v2、2 份：M1 实投 600（应投 500，超投），M3 不投料。
	if _, err := s1.CreateBatch("b2", "B2", "R1", "v2", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.AddFeeding("f-b2", "B2", "M1", "600", time.Now(), "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CloseBatch("c1", "B1"); err != nil {
		t.Fatal(err)
	}

	// 写入侧归属仍然只认批次绑定的版本：只在 v1 的物料不能投给 v2 批次，
	// 完全不存在的物料也一样。
	for _, tc := range []struct{ reqNo, mat string }{
		{"bad-m2", "M2"}, // 只在 v1
		{"bad-m0", "M0"}, // 只在 v1
		{"bad-m9", "M9"}, // 台账中不存在
	} {
		if _, err := s1.AddFeeding(tc.reqNo, "B2", tc.mat, "1", time.Now(), "王五"); !errors.Is(err, ErrMaterialNotInRecipe) {
			t.Fatalf("向 v2 批次投绑定版本之外的物料 %q 应返回 ErrMaterialNotInRecipe，得到 %v",
				tc.mat, err)
		}
	}
	// 失败不占用请求编号：同一编号改投绑定版本内的 M1 应成功，序号连续。
	f, err := s1.AddFeeding("bad-m2", "B2", "M1", "1", time.Now(), "王五")
	if err != nil {
		t.Fatalf("被拒绝的请求编号修正后应可使用: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("修正后投料结果不正确: %+v", f)
	}

	// 关闭并重新打开：欠投/超投/零投料都不构成台账损坏，核对结果逐版本保留。
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("含欠投与超投记录的合法台账应能重新打开: %v", err)
	}
	defer s2.Close()

	v1, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatalf("重新打开后查询已关闭批次失败: %v", err)
	}
	if v1.Status != StatusClosed || v1.RecipeVersion != "v1" {
		t.Fatalf("B1 状态与绑定版本应保留: %+v", v1)
	}
	m1 := materialsMap(v1)
	if len(m1) != 3 {
		t.Fatalf("B1 应只列 v1 的三种物料（不含 v2 新增的 M3），得到 %d 项: %+v", len(m1), m1)
	}
	checkRequirement(t, m1, "M1", "300", "100", "-200") // 欠投只是核对结果
	checkRequirement(t, m1, "M2", "1.5", "0", "-1.5")  // 零实投仍显示
	checkRequirement(t, m1, "M0", "30", "0", "-30")
	if _, ok := m1["M3"]; ok {
		t.Fatalf("v2 才有的 M3 不应出现在 v1 批次的核对中")
	}

	v2, err := s2.GetBatch("B2")
	if err != nil {
		t.Fatalf("重新打开后查询 B2 失败: %v", err)
	}
	if v2.Status != StatusExecuting || v2.RecipeVersion != "v2" {
		t.Fatalf("B2 状态与绑定版本应保留: %+v", v2)
	}
	m2 := materialsMap(v2)
	if len(m2) != 2 {
		t.Fatalf("B2 应只列 v2 的两种物料（不含 v1 的 M2、M0），得到 %d 项: %+v", len(m2), m2)
	}
	// 应投按 v2 的 250 × 2 = 500；实投 600 + 1 = 601，全部按 M1 的实际记录累计。
	checkRequirement(t, m2, "M1", "500", "601", "101")
	checkRequirement(t, m2, "M3", "4", "0", "-4")
}
