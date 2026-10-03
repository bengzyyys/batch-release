package release

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const limitGrams = "9223372036854775.807"

func openExecutingBatch(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	registerStandardRecipe(t, s, "recipe-1") // M1=100, M2=0.5, M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	return s
}

// 累计恰好达到上限可以成功，再追加哪怕 0.001 克也必须失败；
// 查询中的实投与差额必须准确，不得出现负数或错误的小数值。
func TestFeedingCumulativeLimit(t *testing.T) {
	s := openExecutingBatch(t)

	// 先登记 9223372036854775.806 克。
	f1, err := s.AddFeeding("f1", "B1", "M1", "9223372036854775.806", time.Now(), "张三")
	if err != nil {
		t.Fatalf("登记接近上限的数量应成功: %v", err)
	}
	if f1.Seq != 1 || f1.Grams != "9223372036854775.806" {
		t.Fatalf("投料视图不正确: %+v", f1)
	}
	// 再登记 0.001 克，累计恰好等于上限，应成功。
	f2, err := s.AddFeeding("f2", "B1", "M1", "0.001", time.Now(), "张三")
	if err != nil {
		t.Fatalf("累计恰好等于上限应成功: %v", err)
	}
	if f2.Seq != 2 {
		t.Fatalf("第二次投料序号应为 2，得到 %d", f2.Seq)
	}

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	var m1 MaterialRequirement
	for _, m := range view.Materials {
		if m.MaterialNo == "M1" {
			m1 = m
		}
	}
	if m1.ActualGrams != limitGrams {
		t.Fatalf("累计实投应为上限 %s，得到 %s", limitGrams, m1.ActualGrams)
	}
	// 应投 = 100 × 10 = 1000，差额 = 上限 - 1000。
	if m1.RequiredGrams != "1000" {
		t.Fatalf("应投量不正确: %s", m1.RequiredGrams)
	}
	if m1.DifferenceGrams != "9223372036853775.807" {
		t.Fatalf("差额不正确: %s", m1.DifferenceGrams)
	}

	// 之后再登记 0.001 克必须失败。
	_, err = s.AddFeeding("f3", "B1", "M1", "0.001", time.Now(), "张三")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("超过上限应返回 ErrInvalidInput，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "M1") {
		t.Fatalf("错误信息应指明是哪种物料，得到 %v", err)
	}

	// 失败后查询仍看到此前的完整投料与上限值，记录数不变。
	view, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("被拒绝的投料不应保存，得到 %d 条", len(view.Feedings))
	}
	for _, m := range view.Materials {
		if m.MaterialNo == "M1" && m.ActualGrams != limitGrams {
			t.Fatalf("拒绝后累计实投应仍为上限，得到 %s", m.ActualGrams)
		}
	}
}

// 单次输入本身超过上限也必须拒绝，不能整数回绕成较小的合法克数后接受。
func TestSingleFeedingOverflowRejected(t *testing.T) {
	s := openExecutingBatch(t)
	cases := []string{
		"9223372036854775.808",     // 比上限多 0.001 克
		"9223372036854776",         // 整数部分已超上限
		"18446744073709551.615",    // 2×上限，旧实现会回绕成负数
		"18446744073709552",        // 旧实现会回绕成 0.384 克后被接受
		"999999999999999999999999", // 远超 int64
	}
	for i, g := range cases {
		reqNo := "big-" + string(rune('a'+i))
		_, err := s.AddFeeding(reqNo, "B1", "M1", g, time.Now(), "张三")
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("单次输入 %s 应返回 ErrInvalidInput，得到 %v", g, err)
		}
	}

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 0 {
		t.Fatalf("全部被拒绝时不应有投料记录，得到 %d 条", len(view.Feedings))
	}
	for _, m := range view.Materials {
		if m.ActualGrams != "0" {
			t.Fatalf("被拒绝的超大数量不应计入，物料 %s 实投为 %s", m.MaterialNo, m.ActualGrams)
		}
	}

	// 被拒绝的请求编号修正数量后仍可提交成功。
	f, err := s.AddFeeding("big-a", "B1", "M1", "0.384", time.Now(), "张三")
	if err != nil {
		t.Fatalf("失败不应占用请求编号，修正后应成功: %v", err)
	}
	if f.Seq != 1 || f.Grams != "0.384" {
		t.Fatalf("修正后投料视图不正确: %+v", f)
	}
}

// 上限按物料分别判断：其他物料投了多少不影响本次登记，
// 物料之间的超投与欠投互不抵消。
func TestFeedingLimitPerMaterial(t *testing.T) {
	s := openExecutingBatch(t)

	// M1 投到上限。
	if _, err := s.AddFeeding("f1", "B1", "M1", limitGrams, time.Now(), "张三"); err != nil {
		t.Fatalf("M1 投到上限应成功: %v", err)
	}
	// M2 独立投到上限，不应受 M1 影响，也不能把两种物料相加后判断。
	if _, err := s.AddFeeding("f2", "B1", "M2", limitGrams, time.Now(), "李四"); err != nil {
		t.Fatalf("上限应按物料分别判断，M2 投到上限应成功: %v", err)
	}

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]MaterialRequirement{}
	for _, m := range view.Materials {
		got[m.MaterialNo] = m
	}
	if got["M1"].ActualGrams != limitGrams {
		t.Fatalf("M1 累计实投不正确: %s", got["M1"].ActualGrams)
	}
	if got["M2"].ActualGrams != limitGrams {
		t.Fatalf("M2 累计实投不正确: %s", got["M2"].ActualGrams)
	}
	// M3 无投料仍列出；应投 = 0.010 × 10 = 0.1，差额 -0.1，
	// 不被 M1/M2 的超投抵消。
	if got["M3"].ActualGrams != "0" || got["M3"].DifferenceGrams != "-0.1" {
		t.Fatalf("M3 核对结果不正确: %+v", got["M3"])
	}

	// M1 再多投失败并指明 M1；M2 同理，错误信息各自指明物料。
	_, errM1 := s.AddFeeding("f3", "B1", "M1", "0.001", time.Now(), "张三")
	if !errors.Is(errM1, ErrInvalidInput) || !strings.Contains(errM1.Error(), "M1") {
		t.Fatalf("M1 超上限应返回指明 M1 的 ErrInvalidInput，得到 %v", errM1)
	}
	_, errM2 := s.AddFeeding("f4", "B1", "M2", "0.001", time.Now(), "李四")
	if !errors.Is(errM2, ErrInvalidInput) || !strings.Contains(errM2.Error(), "M2") {
		t.Fatalf("M2 超上限应返回指明 M2 的 ErrInvalidInput，得到 %v", errM2)
	}
}

// 超限拒绝不保存记录、不改变批次状态、不占用序号与请求编号。
func TestFeedingOverLimitKeepsStateAndSeq(t *testing.T) {
	s := openExecutingBatch(t)
	fixedTime := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	if _, err := s.AddFeeding("f1", "B1", "M1", "9223372036854775.806", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M1", "0.001", fixedTime, "张三"); err != nil {
		t.Fatal(err)
	}
	// 超限请求失败。
	if _, err := s.AddFeeding("f3", "B1", "M1", "0.001", fixedTime, "张三"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("应返回 ErrInvalidInput，得到 %v", err)
	}

	// 批次仍为执行中；换投另一物料成功，序号连续为 3。
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusExecuting {
		t.Fatalf("失败不应改变批次状态，得到 %s", view.Status)
	}
	f3, err := s.AddFeeding("f3", "B1", "M2", "1", fixedTime, "李四")
	if err != nil {
		t.Fatalf("失败不应占用请求编号，合法投料应成功: %v", err)
	}
	if f3.Seq != 3 {
		t.Fatalf("被拒绝的投料不应占用序号，新投料序号应为 3，得到 %d", f3.Seq)
	}

	// 已成功请求的重复提交仍返回第一次的结果（即使数量现在会导致超限）。
	replay, err := s.AddFeeding("f2", "B1", "M1", "0.001", fixedTime, "张三")
	if err != nil {
		t.Fatalf("重放成功请求应返回原结果: %v", err)
	}
	if replay.Seq != 2 || replay.Grams != "0.001" {
		t.Fatalf("重放结果不正确: %+v", replay)
	}
	view, _ = s.GetBatch("B1")
	if len(view.Feedings) != 3 {
		t.Fatalf("重放不应新增投料，得到 %d 条", len(view.Feedings))
	}
}

// 达到上限的数据关闭重开后仍然完整、准确。
func TestFeedingLimitPersists(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RegisterRecipe("recipe-1", "R1", "v1", "标准配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "100"},
		{MaterialNo: "M2", Grams: "0.5"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.AddFeeding("f1", "B1", "M1", limitGrams, time.Now(), "张三"); err != nil {
		t.Fatalf("投到上限应成功: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	view, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Materials[0].ActualGrams != limitGrams {
		t.Fatalf("重开后累计实投应为上限，得到 %s", view.Materials[0].ActualGrams)
	}
	// 重开后上限校验仍然生效。
	if _, err := s2.AddFeeding("f2", "B1", "M1", "0.001", time.Now(), "张三"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("重开后超限投料仍应被拒绝，得到 %v", err)
	}
	// 另一物料不受影响。
	if _, err := s2.AddFeeding("f3", "B1", "M2", "0.001", time.Now(), "李四"); err != nil {
		t.Fatalf("其他物料应可继续登记: %v", err)
	}
}
