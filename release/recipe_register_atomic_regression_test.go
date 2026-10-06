package release

import (
	"errors"
	"testing"
	"time"
)

// 本文件是配方版本登记“整份成功，或整次提交不留任何登记内容”的回归保障，
// 重点保护物料列表前几项合法、错误出现在后面某项的情况：
//   - 登记在校验全部物料之前就开始逐项处理物料，被拒绝时已处理过的前几项
//     绝不能成为一份只有部分物料的配方版本，也不能占用配方编号+版本号；
//   - 失败不占用请求编号，调用方修正输入后用同一请求编号、同一配方编号与
//     版本号必须能正常登记，不能得到 ErrRequestConflict 或 ErrRecipeExists；
//   - 同编号下原有版本、采用原版本的批次（绑定、计划份数、投料、逐物料
//     数量核对）在失败与修正登记前后保持一致，新版本登记不会让老批次改依
//     新物料，也不能用同编号的旧版本把新版本伪装成已登记。
//
// 既有台账约定：
//   - R1/v1「配方初版」：M1=100、M2=0.5、M3=0.010；
//   - 批次 B1 绑定 R1/v1，计划 3 份，已开始执行并投料 M1=100 克。
//
// 失败提交（请求编号 req-v2，目标 R1/v2「配方二失败稿」）的前三项物料均
// 合法，错误只出现在第四项；修正提交沿用同一请求编号、配方编号与版本号，
// 但名称与前几项物料内容与失败时不同，最终登记只能以这次合法提交为准。

// atomicFixedTime 是 B1 唯一一条投料的登记时间。
var atomicFixedTime = time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)

// setupAtomicRecipeLedger 准备“同编号下已有旧版本且旧版本正被批次采用”的
// 台账：登记 R1/v1，创建计划 3 份的 B1 并开始执行，登记一条 M1=100 克投料。
// 新版本的失败与成功登记都不应改动这里的任何内容。返回台账与数据目录，
// 供关闭重开校验使用。
func setupAtomicRecipeLedger(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) 失败: %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() }) // 已关闭时 Close 为空操作，安全重复调用。
	if _, err := s.RegisterRecipe("req-v1", "R1", "v1", "配方初版", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.010"},
	}); err != nil {
		t.Fatalf("登记旧版本 R1/v1 失败: %v", err)
	}
	if _, err := s.CreateBatch("req-b1", "B1", "R1", "v1", 3); err != nil {
		t.Fatalf("创建采用旧版本的批次失败: %v", err)
	}
	if _, err := s.StartBatch("req-start", "B1"); err != nil {
		t.Fatalf("开始执行批次失败: %v", err)
	}
	if _, err := s.AddFeeding("req-feed", "B1", "M1", "100", atomicFixedTime, "张三"); err != nil {
		t.Fatalf("登记旧版本批次的投料失败: %v", err)
	}
	return s, dir
}

// atomicV1Materials 是 R1/v1 登记成功时固定的物料内容。
func atomicV1Materials() []MaterialView {
	return []MaterialView{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
		{MaterialNo: "M3", Grams: "0.01"}, // 登记可写 0.010，显示按既有规则去尾零
	}
}

// assertOldVersionAndBatchUnchanged 断言同编号下的旧版本与采用它的批次，
// 在新版本登记失败与修正成功之后都保持登记时的原样：
//   - R1/v1 名称、物料顺序、每份用量不变；
//   - B1 仍绑定 R1/v1（不借新版本改依据），计划 3 份、执行中、投料仍是
//     登记过的那一条 M1=100；
//   - 逐物料数量核对仍按 v1 计算：M1 应投 300、实投 100、差额 -200；
//     M2 应投 1.5、实投 0、差额 -1.5；M3 应投 0.03、实投 0、差额 -0.03。
func assertOldVersionAndBatchUnchanged(t *testing.T, st *Store, stage string) {
	t.Helper()
	v1, err := st.GetRecipe("R1", "v1")
	if err != nil {
		t.Fatalf("%s：旧版本 R1/v1 应仍可查，得到错误 %v", stage, err)
	}
	assertRecipeViewExactly(t, v1, "R1", "v1", "配方初版", atomicV1Materials())

	b, err := st.GetBatch("B1")
	if err != nil {
		t.Fatalf("%s：采用旧版本的批次 B1 应仍可查，得到错误 %v", stage, err)
	}
	if b.RecipeNo != "R1" || b.RecipeVersion != "v1" || b.RecipeName != "配方初版" {
		t.Fatalf("%s：批次应保持绑定 R1/v1「配方初版」，得到 %q/%q %q",
			stage, b.RecipeNo, b.RecipeVersion, b.RecipeName)
	}
	if b.PlannedPortions != 3 || b.Status != StatusExecuting {
		t.Fatalf("%s：批次计划份数与状态不应改变，得到 份数=%d 状态=%s",
			stage, b.PlannedPortions, b.Status)
	}
	if len(b.Feedings) != 1 {
		t.Fatalf("%s：批次应保留唯一一条投料，得到 %d 条: %+v",
			stage, len(b.Feedings), b.Feedings)
	}
	f := b.Feedings[0]
	if f.Seq != 1 || f.MaterialNo != "M1" || f.Grams != "100" ||
		!f.Time.Equal(atomicFixedTime) || f.Registrar != "张三" {
		t.Fatalf("%s：原投料内容不应改变，得到 %+v", stage, f)
	}
	m := materialsMap(b)
	checkRequirement(t, m, "M1", "300", "100", "-200")
	checkRequirement(t, m, "M2", "1.5", "0", "-1.5")
	checkRequirement(t, m, "M3", "0.03", "0", "-0.03")
}

// assertV2NotFound 断言 R1/v2 没有留下任何登记内容：同编号下旧版本 v1
// 仍在，但查询 v2 必须是 ErrNotFound——不能返回只有前几项物料的半成品
// 配方，也不能借 v1 的存在把 v2 当成已登记。
func assertV2NotFound(t *testing.T, st *Store, stage string) {
	t.Helper()
	got, err := st.GetRecipe("R1", "v2")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("%s：失败后 R1/v2 应返回 ErrNotFound，得到 err=%v view=%+v", stage, err, got)
	}
	if got != nil {
		t.Fatalf("%s：失败后查询 R1/v2 不应返回任何配方结果，得到 %+v", stage, got)
	}
}

// atomicFailedMaterials 是各类失败提交的物料列表：前三项编号与每份克数全部
// 合法（且与修正提交的前三项内容不同，以便任何失败残留都会被识别），错误
// 只出现在第四项——这正是原子性最容易被破坏的位置。
func atomicFailedMaterials(bad MaterialInput) []MaterialInput {
	return []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "2"},
		{MaterialNo: "M3", Grams: "7"},
		bad,
	}
}

// atomicCorrectedMaterials 是修正后的合法提交：名称与前三项用量都与失败时
// 不同，最后一项 M4 是失败时根本没处理到的新物料；0.001 克是最小合法精度，
// 1.000 克合法且结果必须按既有显示规则返回 "1"。
func atomicCorrectedMaterials() []MaterialInput {
	return []MaterialInput{
		{MaterialNo: "M1", Grams: "200"},
		{MaterialNo: "M2", Grams: "0.25"},
		{MaterialNo: "M3", Grams: "0.001"},
		{MaterialNo: "M4", Grams: "1.000"},
	}
}

// atomicWantV2Materials 是修正提交成功后 R1/v2 唯一允许的物料视图：
// 完整四项、顺序不变、最后一项不漏；数量以本次合法提交为准，不混入失败
// 提交里的旧用量（M2=2、M3=7），1.000 显示为 1、0.001 保留三位小数。
func atomicWantV2Materials() []MaterialView {
	return []MaterialView{
		{MaterialNo: "M1", Grams: "200"},
		{MaterialNo: "M2", Grams: "0.25"},
		{MaterialNo: "M3", Grams: "0.001"},
		{MaterialNo: "M4", Grams: "1"},
	}
}

// TestRecipeRegisterRejectsPartialVersion 固定登记配方版本的原子性：
// 物料列表前几项合法、后面某项出错时，登记返回 ErrInvalidInput 且没有任何
// 成功结果，台账中不留下半成品版本；随后用同一请求编号、配方编号与版本号
// 提交修正后的合法内容（名称与前几项物料均可与失败时不同）必须正常登记，
// 结果与查询都以修正提交为准，旧版本与老批次始终不变。
func TestRecipeRegisterRejectsPartialVersion(t *testing.T) {
	cases := []struct {
		name string
		bad  MaterialInput
	}{
		// 后项物料编号与前项重复：前三项已全部处理过后才撞上重复编号。
		{"后项物料编号与前项重复", MaterialInput{MaterialNo: "M1", Grams: "5"}},
		// 后项每份克数写成四位小数。
		{"后项每份克数四位小数", MaterialInput{MaterialNo: "M4", Grams: "1.2345"}},
		// 四位小数即使最后一位为零也属于非法输入，不能当成 1.000 接受。
		{"后项每份克数四位小数末位为零", MaterialInput{MaterialNo: "M4", Grams: "1.0000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := setupAtomicRecipeLedger(t)

			// 失败提交：前三项合法，错误只在第四项。
			failed := atomicFailedMaterials(tc.bad)
			view, err := s.RegisterRecipe("req-v2", "R1", "v2", "配方二失败稿", failed)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("后项非法的登记应返回 ErrInvalidInput，得到 %v", err)
			}
			if view != nil {
				t.Fatalf("登记失败不应返回任何配方结果，得到 %+v", view)
			}

			// 失败即不留任何登记内容：R1/v2 查无此版本，旧版本与老批次原样。
			assertV2NotFound(t, s, "登记失败后")
			assertOldVersionAndBatchUnchanged(t, s, "登记失败后")

			// 修正输入后沿用同一请求编号、同一配方编号与版本号提交：不得返回
			// ErrRequestConflict（失败不占请求编号）或 ErrRecipeExists（失败
			// 不占版本号）。名称与前几项物料故意与失败时不同。
			view, err = s.RegisterRecipe("req-v2", "R1", "v2", "配方二定稿", atomicCorrectedMaterials())
			if err != nil {
				t.Fatalf("修正输入后用同一请求编号与版本号应正常登记，得到 %v", err)
			}
			assertRecipeViewExactly(t, view, "R1", "v2", "配方二定稿", atomicWantV2Materials())

			// 查询结果与登记结果一致，完整保留物料顺序与用量，不遗漏最后一项，
			// 也不混入失败提交中的旧名称、旧物料或旧数量。
			v2, err := s.GetRecipe("R1", "v2")
			if err != nil {
				t.Fatalf("修正登记后查询 R1/v2 失败: %v", err)
			}
			assertRecipeViewExactly(t, v2, "R1", "v2", "配方二定稿", atomicWantV2Materials())

			// 新版本与原版本各自可查，互不顶替。
			v1, err := s.GetRecipe("R1", "v1")
			if err != nil {
				t.Fatalf("修正登记后旧版本 R1/v1 应仍可查: %v", err)
			}
			assertRecipeViewExactly(t, v1, "R1", "v1", "配方初版", atomicV1Materials())
			// 老批次不会因新版本登记而改用新物料依据。
			assertOldVersionAndBatchUnchanged(t, s, "修正登记后")

			// 新批次采用新版本时，按 v2 的物料顺序与每份用量核对，与 v1 互不
			// 影响：M1 应投 200×2=400，M2 0.25×2=0.5，M3 0.001×2=0.002，
			// M4 1×2=2；B1 仍绑定 v1。
			if _, err := s.CreateBatch("req-b2", "B2", "R1", "v2", 2); err != nil {
				t.Fatalf("新版本应可被新批次采用: %v", err)
			}
			b2, err := s.GetBatch("B2")
			if err != nil {
				t.Fatal(err)
			}
			if b2.RecipeVersion != "v2" || b2.RecipeName != "配方二定稿" {
				t.Fatalf("新批次应绑定 R1/v2「配方二定稿」，得到 %q %q",
					b2.RecipeVersion, b2.RecipeName)
			}
			m2 := materialsMap(b2)
			if len(m2) != 4 {
				t.Fatalf("新批次应按新版本完整列出 4 项物料，得到 %+v", b2.Materials)
			}
			checkRequirement(t, m2, "M1", "400", "0", "-400")
			checkRequirement(t, m2, "M2", "0.5", "0", "-0.5")
			checkRequirement(t, m2, "M3", "0.002", "0", "-0.002")
			checkRequirement(t, m2, "M4", "2", "0", "-2")

			// 成功请求的幂等重放仍返回修正提交第一次成功时的结果。
			replay, err := s.RegisterRecipe("req-v2", "R1", "v2", "配方二定稿", atomicCorrectedMaterials())
			if err != nil {
				t.Fatalf("成功请求按原内容重放应成功: %v", err)
			}
			assertRecipeViewExactly(t, replay, "R1", "v2", "配方二定稿", atomicWantV2Materials())

			// 关闭重开：失败从未落盘，成功登记完整保留；旧版本、新版本与
			// 各自的批次绑定在重新读取后仍保持一致。
			if err := s.Close(); err != nil {
				t.Fatalf("关闭台账失败: %v", err)
			}
			s2, err := Open(dir)
			if err != nil {
				t.Fatalf("重新打开台账失败: %v", err)
			}
			defer s2.Close()
			reopenedV1, err := s2.GetRecipe("R1", "v1")
			if err != nil {
				t.Fatalf("重开后旧版本应可查: %v", err)
			}
			assertRecipeViewExactly(t, reopenedV1, "R1", "v1", "配方初版", atomicV1Materials())
			reopenedV2, err := s2.GetRecipe("R1", "v2")
			if err != nil {
				t.Fatalf("重开后新版本应可查: %v", err)
			}
			assertRecipeViewExactly(t, reopenedV2, "R1", "v2", "配方二定稿", atomicWantV2Materials())
			assertOldVersionAndBatchUnchanged(t, s2, "关闭重开后")
			reopenedB2, err := s2.GetBatch("B2")
			if err != nil {
				t.Fatalf("重开后采用新版本的批次应可查: %v", err)
			}
			if reopenedB2.RecipeVersion != "v2" || reopenedB2.RecipeName != "配方二定稿" {
				t.Fatalf("重开后新批次应仍绑定 R1/v2，得到 %q %q",
					reopenedB2.RecipeVersion, reopenedB2.RecipeName)
			}
		})
	}
}
