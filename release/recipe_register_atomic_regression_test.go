package release

import (
	"errors"
	"testing"
	"time"
)

// 本文件是配方版本登记“整份成功，或整次提交不留下任何登记内容”的回归保障。
//
// 重点保护物料列表前几项合法、错误出现在后面某项的情况：登记在校验完所有
// 物料之前，已经把前几项累积进了正在构造的配方记录。若这份半成品被提前
// 挂进台账或随提交落盘，就会出现两种都不可接受的后果：
//   - 目标编号+版本下多了一份只有前几项物料的不完整配方，查询还可能借同一
//     编号下已有的旧版本把新版本误当成“已登记”；
//   - 本次请求编号被占用，调用方修正输入后用同一请求编号重新提交时，反而
//     拿到 ErrRequestConflict，无法再次登记。
//
// 约定的台账背景：R1/v1「配方初版」已登记，物料顺序固定为
// M1=100 克、M2=0.5 克、M3=0.010 克；批次 B1 采用该版本，计划 3 份，
// 已开始执行并为 M1 投料 120 克。调用方随后要登记尚未存在的 R1/v2，
// 前段物料都合法，错误只出现在后段物料（物料编号与前项重复，或每份克数
// 写成四位小数，含末位为零的 1.0000）。
//
// 这些失败必须：
//   - 返回 ErrInvalidInput 且不返回任何配方结果；
//   - 不留下只有前几项物料的 R1/v2，查询 R1/v2 仍是 ErrNotFound，也不能
//     借 R1/v1 顶替；
//   - 不占用请求编号、不落任何业务记录：关闭重开后同样查不到 R1/v2；
//   - 不动 R1/v1 的名称、物料顺序与每份用量，也不动 B1 的版本绑定、计划
//     份数、投料与逐物料数量核对。
//
// 随后调用方用同一个请求编号、同一个 R1/v2 提交修正后的合法内容（名称与
// 前段物料都可以与失败时不同），必须正常登记，不能返回请求编号冲突或
// 配方版本已存在；最终结果与查询都以这次合法提交为准，完整保留物料顺序与
// 用量，不漏最后一项，也不混入失败提交里的旧物料或旧数量。0.001 克仍可
// 登记，1.000 克仍按既有规则显示为 1 克。

// atomicOldMaterials 是背景版本 R1/v1 登记时的物料内容。
func atomicOldMaterials() []MaterialInput {
	return []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	}
}

// atomicOldMaterialViews 是 R1/v1 应始终呈现的物料视图（顺序与用量固定）。
// 登记时 M3 填 0.010，按既有显示规则去尾零后返回 0.01（与 1.000 返回 1
// 同一规则）。
func atomicOldMaterialViews() []MaterialView {
	return []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.01"},
	}
}

// setupAtomicRegisterLedger 准备背景台账：R1/v1 已登记，批次 B1 采用它
// （计划 3 份、执行中、M1 已投 120 克）。返回数据位置与已打开的台账。
func setupAtomicRegisterLedger(t *testing.T) (string, *Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) 失败: %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.RegisterRecipe("recipe-v1", "R1", "v1", "配方初版", atomicOldMaterials()); err != nil {
		t.Fatalf("登记背景版本 R1/v1 失败: %v", err)
	}
	if _, err := s.CreateBatch("batch-b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatalf("创建背景批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("开始执行背景批次失败: %v", err)
	}
	feedTime := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	if _, err := s.AddFeeding("feed-b1-m1", "B1", "M1", "120", feedTime, "张三"); err != nil {
		t.Fatalf("背景批次登记 M1 投料失败: %v", err)
	}
	return dir, s
}

// assertOldVersionAndBatchUnchanged 断言失败登记与新版本登记都没有触动
// 背景数据：R1/v1 的名称/物料顺序/每份用量不变；B1 仍绑定 R1/v1，计划
// 3 份、执行中、保留那条 M1=120 克投料，逐物料应投/实投/差额不变，且不
// 混入任何新版本物料（M4、M5、M6 都不应出现）。
func assertOldVersionAndBatchUnchanged(t *testing.T, st *Store) {
	t.Helper()
	rv, err := st.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("背景版本 R1/v1 应仍可查询: %v", err)
	}
	assertRecipeViewExactly(t, rv, "R1", "v1", "配方初版", atomicOldMaterialViews())

	b, err := st.GetBatch("B1")
	if err != nil {
		t.Fatalf("背景批次 B1 应仍可查询: %v", err)
	}
	if b.BatchNo != "B1" || b.RecipeNo != "R1" || b.RecipeVersion != "v1" || b.RecipeName != "配方初版" {
		t.Fatalf("批次不应改绑新版本，得到 %q/%q/%q/%q",
			b.BatchNo, b.RecipeNo, b.RecipeVersion, b.RecipeName)
	}
	if b.PlannedPortions != 3 || b.Status != StatusExecuting {
		t.Fatalf("批次计划份数与状态不应变化，得到 份数=%d 状态=%q", b.PlannedPortions, b.Status)
	}
	if len(b.Feedings) != 1 {
		t.Fatalf("背景批次应保留唯一一条投料，得到 %+v", b.Feedings)
	}
	wantTime := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	f := b.Feedings[0]
	if f.Seq != 1 || f.MaterialNo != "M1" || f.Grams != "120" || f.Registrar != "张三" || !f.Time.Equal(wantTime) {
		t.Fatalf("背景投料不应变化，得到 %+v", f)
	}
	if len(b.Materials) != 3 {
		t.Fatalf("批次数量核对应只按 v1 的 3 项物料，不应混入新版本物料，得到 %+v", b.Materials)
	}
	m := materialsMap(b)
	// 100×3=300，实投 120，差额 -180。
	checkRequirement(t, m, "M1", "300", "120", "-180")
	// 0.5×3=1.5，未投料。
	checkRequirement(t, m, "M2", "1.5", "0", "-1.5")
	// 0.010×3=0.03，未投料。
	checkRequirement(t, m, "M3", "0.03", "0", "-0.03")
}

// assertV2NotFound 断言 R1/v2 没有任何登记痕迹：查询必须是 ErrNotFound，
// 不能返回只有前几项物料的半成品，也不能借 R1/v1 顶替成“已登记”。
func assertV2NotFound(t *testing.T, st *Store) {
	t.Helper()
	got, err := st.GetRecipe("R1", "v2")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后 R1/v2 应返回 ErrNotFound，得到 err=%v view=%+v", err, got)
	}
	if got != nil {
		t.Fatalf("失败后不应能取回任何 R1/v2 配方结果，得到 %+v", got)
	}
}

// TestRegisterRecipeAtomicRejectLateMaterialError 用两类“错误出现在后段
// 物料”的提交验证登记的原子性，随后用同一请求编号提交修正内容。
func TestRegisterRecipeAtomicRejectLateMaterialError(t *testing.T) {
	// 前三项均合法，错误只出现在第四项（最后一项），最能暴露部分登记。
	cases := []struct {
		name         string
		badMaterials []MaterialInput
	}{
		{
			name: "末项物料编号与前项重复",
			badMaterials: []MaterialInput{
				{MaterialNo: "M1", Grams: "10"},
				{MaterialNo: "M2", Grams: "0.25"},
				{MaterialNo: "M3", Grams: "0.001"},
				{MaterialNo: "M1", Grams: "5"}, // 与第一项重复
			},
		},
		{
			name: "末项克数四位小数",
			badMaterials: []MaterialInput{
				{MaterialNo: "M1", Grams: "10"},
				{MaterialNo: "M2", Grams: "0.25"},
				{MaterialNo: "M3", Grams: "0.001"},
				{MaterialNo: "M4", Grams: "1.2345"},
			},
		},
		{
			name: "末项克数四位小数且末位为零",
			badMaterials: []MaterialInput{
				{MaterialNo: "M1", Grams: "10"},
				{MaterialNo: "M2", Grams: "0.25"},
				{MaterialNo: "M3", Grams: "0.001"},
				{MaterialNo: "M4", Grams: "1.0000"}, // 数值上等于 1 克，写法仍非法
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, s := setupAtomicRegisterLedger(t)

			// 失败登记：前段物料都已处理过，错误在末项。
			view, err := s.RegisterRecipe("req-new-version", "R1", "v2", "配方改版", tc.badMaterials)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("后段物料非法应返回 ErrInvalidInput，得到 %v", err)
			}
			if view != nil {
				t.Fatalf("登记失败不应返回任何配方结果，得到 %+v", view)
			}

			// 失败当场不留半成品，背景版本与批次不变。
			assertV2NotFound(t, s)
			assertOldVersionAndBatchUnchanged(t, s)

			// 关闭重开：失败整次不落盘，重开后仍查不到 R1/v2，背景不变。
			if err := s.Close(); err != nil {
				t.Fatalf("关闭台账失败: %v", err)
			}
			s2, err := Open(dir)
			if err != nil {
				t.Fatalf("重新打开台账失败: %v", err)
			}
			t.Cleanup(func() { _ = s2.Close() })
			assertV2NotFound(t, s2)
			assertOldVersionAndBatchUnchanged(t, s2)

			// 修正输入：仍使用同一请求编号、同一 R1/v2；名称与前段物料都与
			// 失败时不同。M4=1.000 合法且应显示为 1；M5=0.001 是最小合法用量。
			corrected := []MaterialInput{
				{MaterialNo: "M2", Grams: "2"},
				{MaterialNo: "M4", Grams: "1.000"},
				{MaterialNo: "M5", Grams: "0.001"},
				{MaterialNo: "M6", Grams: "7"}, // 最后一项，必须完整保留
			}
			view, err = s2.RegisterRecipe("req-new-version", "R1", "v2", "配方改订版", corrected)
			if err != nil {
				t.Fatalf("修正后用同一请求编号应正常登记，不能冲突或报版本已存在: %v", err)
			}
			wantViews := []MaterialView{
				{MaterialNo: "M2", Grams: "2"},
				{MaterialNo: "M4", Grams: "1"}, // 1.000 按既有显示规则返回 1
				{MaterialNo: "M5", Grams: "0.001"},
				{MaterialNo: "M6", Grams: "7"},
			}
			assertRecipeViewExactly(t, view, "R1", "v2", "配方改订版", wantViews)

			// 查询以合法提交为准：不漏末项，不混入失败时的 M1=10、M2=0.25、
			// M3=0.001 或任何旧数量。
			queried, err := s2.GetRecipe("R1", "v2")
			if err != nil {
				t.Fatalf("修正后查询 R1/v2 失败: %v", err)
			}
			assertRecipeViewExactly(t, queried, "R1", "v2", "配方改订版", wantViews)

			// 同一请求编号幂等重放修正内容：返回同一份成功结果，不是冲突。
			replay, err := s2.RegisterRecipe("req-new-version", "R1", "v2", "配方改订版", corrected)
			if err != nil {
				t.Fatalf("修正成功后幂等重放应返回首次结果: %v", err)
			}
			assertRecipeViewExactly(t, replay, "R1", "v2", "配方改订版", wantViews)

			// 新版本登记不影响背景：旧版本内容与采用它的批次一切照旧。
			assertOldVersionAndBatchUnchanged(t, s2)

			// 再关闭重开：两个版本各自可查、互不顶替，批次仍以 v1 为依据。
			if err := s2.Close(); err != nil {
				t.Fatalf("二次关闭台账失败: %v", err)
			}
			s3, err := Open(dir)
			if err != nil {
				t.Fatalf("第二次重新打开台账失败: %v", err)
			}
			t.Cleanup(func() { _ = s3.Close() })
			v1, err := s3.GetRecipe("R1", "v1")
			if err != nil {
				t.Fatalf("重开后 R1/v1 应仍可查: %v", err)
			}
			assertRecipeViewExactly(t, v1, "R1", "v1", "配方初版", atomicOldMaterialViews())
			v2, err := s3.GetRecipe("R1", "v2")
			if err != nil {
				t.Fatalf("重开后 R1/v2 应可查: %v", err)
			}
			assertRecipeViewExactly(t, v2, "R1", "v2", "配方改订版", wantViews)
			assertOldVersionAndBatchUnchanged(t, s3)
		})
	}
}

// TestRegisterRecipeAtomicRejectErrorAfterTwoMaterials 再固定一种错误位置：
// 只有两项物料时错误出现在第二项（编号重复 / 四位小数）。这是“前一项已经
// 处理、下一项立刻出错”的最短半成品形态，必须同样整份拒绝、可原样重试。
func TestRegisterRecipeAtomicRejectErrorAfterTwoMaterials(t *testing.T) {
	cases := []struct {
		name string
		bad  []MaterialInput
	}{
		{"第二项与第一项重复", []MaterialInput{
			{MaterialNo: "M8", Grams: "3"},
			{MaterialNo: "M8", Grams: "4"},
		}},
		{"第二项四位小数", []MaterialInput{
			{MaterialNo: "M8", Grams: "3"},
			{MaterialNo: "M9", Grams: "3.0000"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, s := setupAtomicRegisterLedger(t)

			if got, err := s.RegisterRecipe("req-v2-short", "R1", "v2", "配方改版", tc.bad); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("应返回 ErrInvalidInput，得到 err=%v view=%+v", err, got)
			} else if got != nil {
				t.Fatalf("失败不应返回配方结果，得到 %+v", got)
			}
			assertV2NotFound(t, s)
			assertOldVersionAndBatchUnchanged(t, s)

			// 同编号修正为至少三项的合法内容后正常登记。
			corrected := []MaterialInput{
				{MaterialNo: "M8", Grams: "3"},
				{MaterialNo: "M9", Grams: "0.001"},
				{MaterialNo: "M2", Grams: "1.000"},
			}
			view, err := s.RegisterRecipe("req-v2-short", "R1", "v2", "配方改订版", corrected)
			if err != nil {
				t.Fatalf("同编号修正后应能登记: %v", err)
			}
			assertRecipeViewExactly(t, view, "R1", "v2", "配方改订版", []MaterialView{
				{MaterialNo: "M8", Grams: "3"},
				{MaterialNo: "M9", Grams: "0.001"},
				{MaterialNo: "M2", Grams: "1"},
			})
			assertOldVersionAndBatchUnchanged(t, s)
		})
	}
}
