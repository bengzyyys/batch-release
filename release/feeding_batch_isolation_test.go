package release

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// openTwoExecutingBatches 准备同一台账中两个执行中的批次：B1（10 份）与
// B2（3 份）绑定同一个配方版本 R1/v1（M1=100、M2=0.5、M3=0.010），
// 登记的物料编号完全相同。用于核对同一台账里不同批次各自计算同一物料
// 累计上限的规则。
func openTwoExecutingBatches(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1") // M1=100, M2=0.5, M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	return s
}

// feedingSeqs 返回批次查询视图中全部投料的登记序号。
func feedingSeqs(v *BatchView) []int {
	seqs := make([]int, 0, len(v.Feedings))
	for _, f := range v.Feedings {
		seqs = append(seqs, f.Seq)
	}
	return seqs
}

// 两个执行中的批次采用同一配方版本、登记同一物料编号，交替投料时各自的
// 累计实投只包含本批次的投料：应投量按各自份数计算，累计实投与差额只
// 反映各自实际登记的数量；向一批追加不改变另一批的查询结果；两批各自
// 从序号 1 开始，按自己的成功登记顺序连续编号，不因另一批增加投料而跳号。
func TestFeedingAccumulationIsolatedBetweenBatches(t *testing.T) {
	s := openTwoExecutingBatches(t)
	fixedTime := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	// 交替向两批登记同一物料 M1。
	f1, err := s.AddFeeding("f1", "B1", "M1", "250", fixedTime, "张三")
	if err != nil {
		t.Fatal(err)
	}
	if f1.Seq != 1 {
		t.Fatalf("B1 第一次投料序号应为 1，得到 %d", f1.Seq)
	}
	f2, err := s.AddFeeding("f2", "B2", "M1", "120.5", fixedTime, "李四")
	if err != nil {
		t.Fatal(err)
	}
	if f2.Seq != 1 {
		t.Fatalf("B2 应有自己的序号序列，第一次投料序号应为 1，得到 %d", f2.Seq)
	}

	// 向 B1 追加后，B2 的投料条数、实投量和差额都应保持原值。
	f3, err := s.AddFeeding("f3", "B1", "M1", "100", fixedTime, "张三")
	if err != nil {
		t.Fatal(err)
	}
	if f3.Seq != 2 {
		t.Fatalf("B1 第二次投料序号应为 2，得到 %d", f3.Seq)
	}
	viewB2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	if len(viewB2.Feedings) != 1 {
		t.Fatalf("向 B1 追加不应改变 B2 的投料条数，得到 %d 条", len(viewB2.Feedings))
	}
	// B2 应投 = 100 × 3 = 300，实投只含自己的 120.5。
	checkRequirement(t, materialsMap(viewB2), "M1", "300", "120.5", "-179.5")

	// 继续向 B2 投料，只更新 B2；B1 的查询结果保持原值。
	f4, err := s.AddFeeding("f4", "B2", "M1", "0.5", fixedTime, "李四")
	if err != nil {
		t.Fatal(err)
	}
	if f4.Seq != 2 {
		t.Fatalf("B2 第二次投料序号应接着自己的序列编号为 2，得到 %d", f4.Seq)
	}
	viewB1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(viewB1.Feedings) != 2 {
		t.Fatalf("向 B2 追加不应改变 B1 的投料条数，得到 %d 条", len(viewB1.Feedings))
	}
	// B1 应投 = 100 × 10 = 1000，实投只含自己的 250 + 100 = 350。
	checkRequirement(t, materialsMap(viewB1), "M1", "1000", "350", "-650")

	// B2 现在的实投为 120.5 + 0.5 = 121，仍只按自己的登记累计。
	viewB2, err = s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	checkRequirement(t, materialsMap(viewB2), "M1", "300", "121", "-179")

	// 两批各有序号 1、2 的记录，各自连续编号，不因另一批的投料跳号。
	seqsB1, seqsB2 := feedingSeqs(viewB1), feedingSeqs(viewB2)
	if len(seqsB1) != 2 || seqsB1[0] != 1 || seqsB1[1] != 2 {
		t.Fatalf("B1 的登记序号应为 [1 2]，得到 %v", seqsB1)
	}
	if len(seqsB2) != 2 || seqsB2[0] != 1 || seqsB2[1] != 2 {
		t.Fatalf("B2 的登记序号应为 [1 2]，得到 %v", seqsB2)
	}
	if viewB1.Status != StatusExecuting || viewB2.Status != StatusExecuting {
		t.Fatalf("两批应仍为执行中，得到 B1=%s B2=%s", viewB1.Status, viewB2.Status)
	}
}

// 一批先投到距离上限还差 0.001 克，再补足 0.001 克应成功，查询中必须
// 精确显示达到上限的实投量，不能舍入、回绕或把它算进另一批；此时另一批
// 仍可在自己的剩余范围内登记同一物料，哪怕两批的合计数量早已超过单批
// 上限。达到上限的批次再追加 0.001 克应返回指明该物料的 ErrInvalidInput，
// 本次投料不进入记录，原有数量核对与执行中状态保持不变；被拒绝的登记也
// 不改变另一批的已有记录，不妨碍它随后的一次合法投料。
func TestFeedingLimitBoundaryIsolatedBetweenBatches(t *testing.T) {
	s := openTwoExecutingBatches(t)
	fixedTime := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	// B1 先投到距离上限还差 0.001 克，再补足 0.001 克，恰好达到上限。
	if _, err := s.AddFeeding("f1", "B1", "M1", "9223372036854775.806", fixedTime, "张三"); err != nil {
		t.Fatalf("登记接近上限的数量应成功: %v", err)
	}
	f2, err := s.AddFeeding("f2", "B1", "M1", "0.001", fixedTime, "张三")
	if err != nil {
		t.Fatalf("累计恰好等于上限应成功: %v", err)
	}
	if f2.Seq != 2 {
		t.Fatalf("B1 第二次投料序号应为 2，得到 %d", f2.Seq)
	}

	// 查询必须精确显示达到上限的实投量：不舍入、不回绕。
	viewB1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	checkRequirement(t, materialsMap(viewB1), "M1", "1000", limitGrams, "9223372036853775.807")

	// B2 仍可在自己的剩余范围内登记同一物料：两批合计早已超过单批上限，
	// 也不能被当作其中某个批次超限。
	f3, err := s.AddFeeding("f3", "B2", "M1", "500", fixedTime, "李四")
	if err != nil {
		t.Fatalf("另一批达到上限不应拒绝本批的合法投料: %v", err)
	}
	if f3.Seq != 1 {
		t.Fatalf("B2 第一次投料序号应为 1，得到 %d", f3.Seq)
	}

	// 达到上限的 B1 再追加 0.001 克，应返回指明 M1 的 ErrInvalidInput。
	_, err = s.AddFeeding("f4", "B1", "M1", "0.001", fixedTime, "张三")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("超过上限应返回 ErrInvalidInput，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "M1") {
		t.Fatalf("错误信息应指明是哪种物料，得到 %v", err)
	}

	// 被拒绝的投料不进入记录：B1 的数量核对与执行中状态保持不变。
	viewB1, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(viewB1.Feedings) != 2 {
		t.Fatalf("被拒绝的投料不应保存，B1 应有 2 条投料，得到 %d 条", len(viewB1.Feedings))
	}
	if viewB1.Status != StatusExecuting {
		t.Fatalf("被拒绝的投料不应改变批次状态，得到 %s", viewB1.Status)
	}
	checkRequirement(t, materialsMap(viewB1), "M1", "1000", limitGrams, "9223372036853775.807")

	// 被拒绝的登记也不改变另一批的已有记录。
	viewB2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	if len(viewB2.Feedings) != 1 {
		t.Fatalf("B1 的被拒登记不应改变 B2 的投料条数，得到 %d 条", len(viewB2.Feedings))
	}
	checkRequirement(t, materialsMap(viewB2), "M1", "300", "500", "200")

	// 也不妨碍 B2 随后的一次合法投料：返回的序号及查询数量以 B2 自己的
	// 成功记录为准。
	f5, err := s.AddFeeding("f5", "B2", "M1", "0.001", fixedTime, "李四")
	if err != nil {
		t.Fatalf("另一批的被拒登记不应妨碍本批的合法投料: %v", err)
	}
	if f5.Seq != 2 {
		t.Fatalf("B2 第二次投料序号应接着自己的序列编号为 2，得到 %d", f5.Seq)
	}
	viewB2, err = s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	checkRequirement(t, materialsMap(viewB2), "M1", "300", "500.001", "200.001")

	// B2 的追加不影响 B1：B1 仍只有自己的 2 条投料，实投仍为上限。
	viewB1, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(viewB1.Feedings) != 2 {
		t.Fatalf("向 B2 追加不应改变 B1 的投料条数，得到 %d 条", len(viewB1.Feedings))
	}
	checkRequirement(t, materialsMap(viewB1), "M1", "1000", limitGrams, "9223372036853775.807")
}

// 超出应投量继续属于合法投料：计划所需数量（每份克数 × 份数）不是累计
// 实投上限，累计上限仍是 9223372036854775.807 克。两个批次可以各自
// 投到上限，两批实投相加超过上限也不被当作其中某个批次超限。
func TestFeedingBeyondRequiredIsLegalInEachBatch(t *testing.T) {
	s := openTwoExecutingBatches(t)
	fixedTime := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	// B1 应投 = 100 × 10 = 1000，投 1500 已超出应投量，仍是合法投料。
	if _, err := s.AddFeeding("f1", "B1", "M1", "1500", fixedTime, "张三"); err != nil {
		t.Fatalf("超出应投量仍属合法投料: %v", err)
	}
	viewB1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	checkRequirement(t, materialsMap(viewB1), "M1", "1000", "1500", "500")

	// B2 应投只有 300，但应投量不是累计上限：B2 可以一次投到上限。
	if _, err := s.AddFeeding("f2", "B2", "M1", limitGrams, fixedTime, "李四"); err != nil {
		t.Fatalf("应投量不是累计上限，投到上限应成功: %v", err)
	}
	viewB2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	checkRequirement(t, materialsMap(viewB2), "M1", "300", limitGrams, "9223372036854475.807")

	// B1 也可以在自己的剩余范围内补足到上限：两批同时达到上限、合计
	// 为上限的两倍，不被当作任何一批超限。
	if _, err := s.AddFeeding("f3", "B1", "M1", "9223372036853275.807", fixedTime, "张三"); err != nil {
		t.Fatalf("另一批已到上限不影响本批投到自己的上限: %v", err)
	}
	viewB1, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	checkRequirement(t, materialsMap(viewB1), "M1", "1000", limitGrams, "9223372036853775.807")

	// 两批各自再追加 0.001 克才各自超限，错误信息各自指明物料。
	_, errB1 := s.AddFeeding("f4", "B1", "M1", "0.001", fixedTime, "张三")
	if !errors.Is(errB1, ErrInvalidInput) || !strings.Contains(errB1.Error(), "M1") {
		t.Fatalf("B1 超上限应返回指明 M1 的 ErrInvalidInput，得到 %v", errB1)
	}
	_, errB2 := s.AddFeeding("f5", "B2", "M1", "0.001", fixedTime, "李四")
	if !errors.Is(errB2, ErrInvalidInput) || !strings.Contains(errB2.Error(), "M1") {
		t.Fatalf("B2 超上限应返回指明 M1 的 ErrInvalidInput，得到 %v", errB2)
	}
}
