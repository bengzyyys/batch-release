package release

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件是“登记投料累计上限按批次隔离”的回归保障：重点保护同一台账里两个
// 执行中的批次各自独立计算同一物料累计实投上限的规则。累计范围始终只是
// “一个批次中的一种物料”：两个批次即使采用同一个配方版本、登记的物料编号
// 也相同，各自的累计实投仍只包含本批次的投料，序号也各自从 1 开始连续
// 编号。一个批次已经达到上限，不能使另一个尚有余量的批次被拒绝；两个批次
// 的实投相加早已超过单批上限，也不能被当作其中某个批次超限。数量边界沿用
// 千分之一克精度与 9223372036854775.807 克上限：一批先投到距上限还差
// 0.001 克再补足 0.001 克应精确达到上限，不能舍入、回绕或算进另一批；
// 超出应投量继续属于合法投料，计划所需数量不是累计实投上限。已达上限的
// 批次再追加 0.001 克返回现有的 ErrInvalidInput（错误信息指出该物料），
// 本次投料不进入记录、不占用请求编号，批次保持执行中；这次拒绝不能改变
// 另一批的已有记录，也不能妨碍另一批随后的一次合法投料。本文件只补充现有
// 功能的自动化检查，不改变公开入口、成功结果与错误分类。
//
// 复用 feeding_limit_test.go 的 limitGrams（9223372036854775.807），
// 以及 materialsMap / checkRequirement / mustGetBatch 等查询断言助手。

// openTwoExecutingBatches 建立同一台账里两个都采用 R1/v1 的执行中批次：
//   - R1/v1：M1 每份 0.001 克、M2 每份 1 克（M1 的应投量因此远小于累计
//     上限，用于证明计划所需数量不是累计实投上限——实投可以远超应投）；
//   - B1 计划 2 份：M1 应投 0.002、M2 应投 2；
//   - B2 计划 3 份：M1 应投 0.003、M2 应投 3。
//
// 两批采用同一配方版本、份数不同，查询应投量必须分别按各自份数计算。
func openTwoExecutingBatches(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("iso-recipe", "R1", "v1", "隔离配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "0.001"},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatalf("登记 R1/v1 失败: %v", err)
	}
	if _, err := s.CreateBatch("iso-b1-create", "B1", "R1", "v1", 2); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	if _, err := s.CreateBatch("iso-b2-create", "B2", "R1", "v1", 3); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	if _, err := s.StartBatch("iso-b1-start", "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	if _, err := s.StartBatch("iso-b2-start", "B2"); err != nil {
		t.Fatalf("开始执行 B2 失败: %v", err)
	}
	return s
}

// 两个执行中的批次交替登记同一种物料，累计上限必须严格按批次各自判断：
// B1 精确投到上限不影响尚有余量的 B2；两批实投合计远超单批上限时，B2 仍
// 可在自己的剩余范围内继续登记同一物料。B1 达上限后追加 0.001 克被拒绝
// （ErrInvalidInput 且信息指出 M1），既不进入 B1 的记录、不改变 B1 的
// 数量核对与执行中状态，也不改变 B2 的已有记录或妨碍 B2 随后合法投料。
func TestFeedingCumulativeLimitIsolatedAcrossExecutingBatches(t *testing.T) {
	s := openTwoExecutingBatches(t)
	fixedTime := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

	// 1) B1 先投到距上限还差 0.001 克（本批次自己的第 1 条）。
	f1, err := s.AddFeeding("iso-b1-a", "B1", "M1", "9223372036854775.806", fixedTime, "张三")
	if err != nil {
		t.Fatalf("B1 登记距上限差 0.001 克的数量应成功: %v", err)
	}
	if f1.Seq != 1 || f1.MaterialNo != "M1" || f1.Grams != "9223372036854775.806" {
		t.Fatalf("B1 第一条投料视图不正确: %+v", f1)
	}

	// 2) 交替：B2 登记同一种物料 M1，序号同样从 1 开始——不同批次各自编号。
	f2, err := s.AddFeeding("iso-b2-a", "B2", "M1", "0.001", fixedTime, "李四")
	if err != nil {
		t.Fatalf("B2 登记 M1 应成功（B1 的大额投料不影响 B2）: %v", err)
	}
	if f2.Seq != 1 || f2.Grams != "0.001" {
		t.Fatalf("B2 第一条投料序号应为 1，得到 %+v", f2)
	}

	// 3) B1 补足最后 0.001 克，本批次累计恰好等于上限，应成功。
	f3, err := s.AddFeeding("iso-b1-b", "B1", "M1", "0.001", fixedTime, "张三")
	if err != nil {
		t.Fatalf("B1 累计恰好等于上限应成功: %v", err)
	}
	if f3.Seq != 2 {
		t.Fatalf("B1 第二条投料序号应为 2，得到 %d", f3.Seq)
	}

	// B1 查询必须精确显示达到上限的实投量：不能舍入、不能回绕；
	// 应投量仍只按 B1 自己的 2 份计算（0.002），超投合法，差额如实为正。
	b1 := mustGetBatch(t, s, "B1")
	if b1.Status != StatusExecuting {
		t.Fatalf("B1 应仍为执行中，得到 %s", b1.Status)
	}
	if len(b1.Feedings) != 2 {
		t.Fatalf("B1 应有 2 条投料，得到 %d", len(b1.Feedings))
	}
	checkRequirement(t, materialsMap(b1), "M1", "0.002", limitGrams, "9223372036854775.805")
	checkRequirement(t, materialsMap(b1), "M2", "2", "0", "-2")

	// 4) 向 B1 追加成功后，B2 查询得到的投料条数、实投量和差额都保持原值，
	//    应投量按 B2 自己的 3 份（0.003）计算，与 B1 的份数和实投无关。
	b2 := mustGetBatch(t, s, "B2")
	if len(b2.Feedings) != 1 || b2.Feedings[0].Seq != 1 || b2.Feedings[0].Grams != "0.001" {
		t.Fatalf("B1 追加后 B2 应仍只有自己的 1 条 0.001 克投料，得到 %+v", b2.Feedings)
	}
	checkRequirement(t, materialsMap(b2), "M1", "0.003", "0.001", "-0.002")
	checkRequirement(t, materialsMap(b2), "M2", "3", "0", "-3")

	// 5) B1 已达上限，再追加 0.001 克必须返回 ErrInvalidInput，
	//    错误信息能指出物料 M1。
	_, err = s.AddFeeding("iso-b1-over", "B1", "M1", "0.001", fixedTime, "张三")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("B1 超过上限应返回 ErrInvalidInput，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "M1") {
		t.Fatalf("超限错误信息应指出物料 M1，得到 %v", err)
	}

	// 被拒绝的投料不进入 B1 的记录：条数、序号、实投、差额与执行中状态不变。
	b1 = mustGetBatch(t, s, "B1")
	if b1.Status != StatusExecuting {
		t.Fatalf("被拒绝的投料不应改变 B1 状态，得到 %s", b1.Status)
	}
	if len(b1.Feedings) != 2 || b1.Feedings[0].Seq != 1 || b1.Feedings[1].Seq != 2 {
		t.Fatalf("被拒绝的投料不应进入记录，B1 得到 %+v", b1.Feedings)
	}
	checkRequirement(t, materialsMap(b1), "M1", "0.002", limitGrams, "9223372036854775.805")

	// 6) B1 已达上限、且两批 M1 实投合计（上限 + 0.001）已经超过单批上限，
	//    都不能使尚有余量的 B2 被拒绝：B2 随后的一次合法投料成功，序号与
	//    查询数量只以 B2 自己的成功记录为准（第 2 条）。
	f4, err := s.AddFeeding("iso-b2-b", "B2", "M1", "0.001", fixedTime, "李四")
	if err != nil {
		t.Fatalf("B1 达上限不应妨碍 B2 继续投料: %v", err)
	}
	if f4.Seq != 2 || f4.Grams != "0.001" {
		t.Fatalf("B2 第二条投料序号应为 2（不因 B1 已有 2 条而跳号），得到 %+v", f4)
	}
	b2 = mustGetBatch(t, s, "B2")
	if len(b2.Feedings) != 2 {
		t.Fatalf("B2 应有 2 条投料，得到 %d", len(b2.Feedings))
	}
	checkRequirement(t, materialsMap(b2), "M1", "0.003", "0.002", "-0.001")
	// 继续向 B2 投料只更新 B2：B1 的条数与数量保持原值。
	b1 = mustGetBatch(t, s, "B1")
	if len(b1.Feedings) != 2 {
		t.Fatalf("向 B2 投料不应改变 B1 的投料条数，得到 %d", len(b1.Feedings))
	}
	checkRequirement(t, materialsMap(b1), "M1", "0.002", limitGrams, "9223372036854775.805")

	// 7) 被 B1 拒绝的请求编号没有被占用：同一编号改投 B1 自己尚有余量的
	//    M2 应当成功，序号按 B1 自己的序列继续为 3。
	f5, err := s.AddFeeding("iso-b1-over", "B1", "M2", "1", fixedTime, "张三")
	if err != nil {
		t.Fatalf("失败不应占用请求编号，合法投料应成功: %v", err)
	}
	if f5.Seq != 3 {
		t.Fatalf("B1 新投料序号应为 3（被拒绝的登记不占序号），得到 %d", f5.Seq)
	}

	// 8) B2 在自己的剩余范围内登记同一物料 M1，一次补足到 B2 自己的上限：
	//    此时两批 M1 实投合计约为单批上限的两倍，也不能被当作任一批超限。
	f6, err := s.AddFeeding("iso-b2-c", "B2", "M1", "9223372036854775.805", fixedTime, "李四")
	if err != nil {
		t.Fatalf("B2 在自己剩余范围内登记到本批次上限应成功（两批合计超单批上限也不影响）: %v", err)
	}
	if f6.Seq != 3 {
		t.Fatalf("B2 第三条投料序号应为 3，得到 %d", f6.Seq)
	}

	// 两批最终核对：条数与序号各自连续（两批都各有序号 1，互不跳号），
	// 实投各自独立精确累计，应投量分别按各自份数，差额只反映本批次。
	b1 = mustGetBatch(t, s, "B1")
	b2 = mustGetBatch(t, s, "B2")
	for name, v := range map[string]*BatchView{"B1": b1, "B2": b2} {
		if len(v.Feedings) != 3 {
			t.Fatalf("%s 应有 3 条投料，得到 %d", name, len(v.Feedings))
		}
		for i, f := range v.Feedings {
			if f.Seq != i+1 {
				t.Fatalf("%s 第 %d 条投料序号应为 %d（两批各自连续编号），得到 %d",
					name, i+1, i+1, f.Seq)
			}
		}
	}
	// B1：M1 在上限（2 份应投仅 0.002），M2 投了 1（应投 2）。
	checkRequirement(t, materialsMap(b1), "M1", "0.002", limitGrams, "9223372036854775.805")
	checkRequirement(t, materialsMap(b1), "M2", "2", "1", "-1")
	// B2：M1 也在自己的上限（3 份应投仅 0.003），M2 未投；与 B1 互不相加。
	checkRequirement(t, materialsMap(b2), "M1", "0.003", limitGrams, "9223372036854775.804")
	checkRequirement(t, materialsMap(b2), "M2", "3", "0", "-3")

	// 两批各自达到自己的上限后，再追加 0.001 克都按各自批次的累计被拒绝，
	// 错误信息分别指出同一物料 M1——一批已达上限不会代替另一批放行或拒绝。
	for _, batchNo := range []string{"B1", "B2"} {
		reqNo := "iso-" + strings.ToLower(batchNo) + "-over-again"
		if _, err := s.AddFeeding(reqNo, batchNo, "M1", "0.001", fixedTime, "张三"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s 达上限后追加应返回 ErrInvalidInput，得到 %v", batchNo, err)
		} else if !strings.Contains(err.Error(), "M1") {
			t.Fatalf("%s 的超限错误信息应指出 M1，得到 %v", batchNo, err)
		}
	}

	// 持久化后重新打开：两批各自的条数、序号与精确到上限的实投量仍然独立
	// 完整，重开后按批次的超限判断继续生效。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(s.dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	defer s2.Close()
	reopenedB1 := mustGetBatch(t, s2, "B1")
	reopenedB2 := mustGetBatch(t, s2, "B2")
	if len(reopenedB1.Feedings) != 3 || len(reopenedB2.Feedings) != 3 {
		t.Fatalf("重开后两批应各有 3 条投料，得到 B1=%d B2=%d",
			len(reopenedB1.Feedings), len(reopenedB2.Feedings))
	}
	checkRequirement(t, materialsMap(reopenedB1), "M1", "0.002", limitGrams, "9223372036854775.805")
	checkRequirement(t, materialsMap(reopenedB2), "M1", "0.003", limitGrams, "9223372036854775.804")
	if _, err := s2.AddFeeding("iso-b1-after-reopen", "B1", "M1", "0.001", fixedTime, "张三"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("重开后 B1 超限投料仍应被拒绝，得到 %v", err)
	}
	// B1 被拒不影响重开后 B2 的已有记录（B2 同样已达上限，只做查询核对）。
	reopenedB2 = mustGetBatch(t, s2, "B2")
	if len(reopenedB2.Feedings) != 3 {
		t.Fatalf("B1 在重开后被拒绝不应改变 B2 的记录，得到 %d 条", len(reopenedB2.Feedings))
	}
}

// 普通数量下两个批次交替登记同一种物料的完整使用过程：两批采用不同的计划
// 份数，应投量分别按各自份数计算，累计实投与差额只反映各自实际登记的数量；
// 向一批追加只更新该批，另一批查询的条数、实投与差额保持原值。两批可以各
// 有序号为 1 的记录，之后各自按成功登记顺序连续编号，不因另一批增加投料而
// 跳号。超出应投量继续属于合法投料。
func TestTwoExecutingBatchesAlternateFeedingKeepsOwnTotalsAndSeq(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterRecipe("alt-recipe", "R1", "v1", "交替配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "10"},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatalf("登记配方失败: %v", err)
	}
	// B1 计划 2 份：M1 应投 20、M2 应投 2；B2 计划 5 份：M1 应投 50、M2 应投 5。
	if _, err := s.CreateBatch("alt-b1-create", "B1", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("alt-b2-create", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("alt-b1-start", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("alt-b2-start", "B2"); err != nil {
		t.Fatal(err)
	}
	fixedTime := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)

	// 两批严格交替登记，物料编号相同也互不混入对方累计。
	steps := []struct {
		reqNo, batchNo, materialNo, grams string
		wantSeq                           int
	}{
		{"alt-1", "B1", "M1", "5", 1},
		{"alt-2", "B2", "M1", "50", 1}, // 两批各有序号为 1 的记录
		{"alt-3", "B1", "M1", "5", 2},
		{"alt-4", "B2", "M2", "2", 2},
		{"alt-5", "B1", "M2", "2", 3},
		{"alt-6", "B2", "M1", "25", 3}, // B2 的 M1 达到 75，超过应投 50 仍合法
	}
	for _, stp := range steps {
		f, err := s.AddFeeding(stp.reqNo, stp.batchNo, stp.materialNo, stp.grams, fixedTime, "登记人")
		if err != nil {
			t.Fatalf("交替投料 %s→%s %s 克应成功: %v", stp.reqNo, stp.batchNo, stp.grams, err)
		}
		if f.Seq != stp.wantSeq {
			t.Fatalf("%s 的投料序号应为 %d，得到 %d", stp.batchNo, stp.wantSeq, f.Seq)
		}
	}

	b1 := mustGetBatch(t, s, "B1")
	b2 := mustGetBatch(t, s, "B2")
	// 条数与序号：两批各自 3 条、各自从 1 连续编号，不因另一批投料而跳号。
	if len(b1.Feedings) != 3 || len(b2.Feedings) != 3 {
		t.Fatalf("两批应各有 3 条投料，得到 B1=%d B2=%d", len(b1.Feedings), len(b2.Feedings))
	}
	wantB1 := []struct {
		materialNo, grams string
	}{{"M1", "5"}, {"M1", "5"}, {"M2", "2"}}
	wantB2 := []struct {
		materialNo, grams string
	}{{"M1", "50"}, {"M2", "2"}, {"M1", "25"}}
	for i := range wantB1 {
		if b1.Feedings[i].Seq != i+1 || b1.Feedings[i].MaterialNo != wantB1[i].materialNo ||
			b1.Feedings[i].Grams != wantB1[i].grams {
			t.Fatalf("B1 第 %d 条投料不正确: %+v", i+1, b1.Feedings[i])
		}
		if b2.Feedings[i].Seq != i+1 || b2.Feedings[i].MaterialNo != wantB2[i].materialNo ||
			b2.Feedings[i].Grams != wantB2[i].grams {
			t.Fatalf("B2 第 %d 条投料不正确: %+v", i+1, b2.Feedings[i])
		}
	}

	// 数量核对分别按各自份数与各自投料计算：
	// B1（2 份）：M1 应20/实10/差-10，M2 应2/实2/差0；
	// B2（5 份）：M1 应50/实75/差25（超应投合法），M2 应5/实2/差-3。
	// 两批 M1 实投合计 85，但任何一批的实投与差额都不含另一批的投料。
	m1 := materialsMap(b1)
	checkRequirement(t, m1, "M1", "20", "10", "-10")
	checkRequirement(t, m1, "M2", "2", "2", "0")
	m2 := materialsMap(b2)
	checkRequirement(t, m2, "M1", "50", "75", "25")
	checkRequirement(t, m2, "M2", "5", "2", "-3")

	// 再向 B1 追加一次，只更新 B1：B2 的条数、序号与数量核对保持原值。
	if _, err := s.AddFeeding("alt-b1-extra", "B1", "M1", "10", fixedTime, "登记人"); err != nil {
		t.Fatalf("向 B1 追加应成功: %v", err)
	}
	b1 = mustGetBatch(t, s, "B1")
	if len(b1.Feedings) != 4 || b1.Feedings[3].Seq != 4 {
		t.Fatalf("B1 第 4 条应序号 4，得到 %+v", b1.Feedings)
	}
	checkRequirement(t, materialsMap(b1), "M1", "20", "20", "0")
	b2 = mustGetBatch(t, s, "B2")
	if len(b2.Feedings) != 3 {
		t.Fatalf("向 B1 追加不应改变 B2 的投料条数，得到 %d", len(b2.Feedings))
	}
	checkRequirement(t, materialsMap(b2), "M1", "50", "75", "25")
	checkRequirement(t, materialsMap(b2), "M2", "5", "2", "-3")
}
