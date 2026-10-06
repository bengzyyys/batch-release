package release

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是“投料内容核对规则在投料请求结果与关闭结果两处共用同一处”的回归
// 保障：物料编号与登记人精确匹配，克数比较实际数量（1 与 1.000 相同），时间
// 比较同一时刻（不区分时区写法），任一项不同都不接受，即使数量合计相同；
// 克数写法非法与字段内容不符是两类可区分的损坏原因。前半部分直接核对共用
// 规则本身，后半部分经 Open 的损坏路径验证两种场景的定位信息与错误分类。

// 同一份投料内容在物料、克数、时间、登记人四个维度上与自身一致：克数的不同
// 合法写法（1.000 与 1）、同一时刻的不同时区写法都不构成差异。
func TestCompareFeedingContentTreatsEquivalentWritesAsSame(t *testing.T) {
	base := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	otherZone := base.In(time.FixedZone("UTC+8", 8*3600))

	a, ok := feedingContentFromView(FeedingView{Seq: 1, MaterialNo: "M1", Grams: "1.000", Time: base, Registrar: "张三"})
	if !ok {
		t.Fatalf("1.000 应能解析为合法克数")
	}
	b, ok := feedingContentFromView(FeedingView{Seq: 1, MaterialNo: "M1", Grams: "1", Time: otherZone, Registrar: "张三"})
	if !ok {
		t.Fatalf("1 应能解析为合法克数")
	}
	if compareFeedingContent(a, b).anyMismatch() {
		t.Fatalf("1 与 1.000、同一时刻的不同时区写法应视为相同投料内容")
	}
	// 台账实际投料记录（定点数量）与保存结果的字符串写法也走同一处比较。
	rec := feedingContentFromRecord(feedingRecord{Seq: 1, MaterialNo: "M1", GramsMilli: 1000, Time: base, Registrar: "张三"})
	if compareFeedingContent(a, rec).anyMismatch() {
		t.Fatalf("保存结果与实际投料记录应在同一处规则下一致")
	}
}

// 物料、克数、时间、登记人任一不同都必须标记为对应维度不一致；即使另一条
// 投料克数合计相同，单条内容对不上仍不能接受。
func TestCompareFeedingContentReportsEachDimension(t *testing.T) {
	base := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	ref, _ := feedingContentFromView(FeedingView{MaterialNo: "M1", Grams: "1.000", Time: base, Registrar: "张三"})

	cases := []struct {
		name    string
		view    FeedingView
		wantFn  func(feedingMismatch) bool
		wantDim string
	}{
		{"物料不同", FeedingView{MaterialNo: "M2", Grams: "1", Time: base, Registrar: "张三"},
			func(m feedingMismatch) bool { return m.material && !m.grams && !m.time && !m.registrar }, "物料"},
		{"克数不同", FeedingView{MaterialNo: "M1", Grams: "2", Time: base, Registrar: "张三"},
			func(m feedingMismatch) bool { return m.grams && !m.material && !m.time && !m.registrar }, "克数"},
		{"时间不是同一时刻", FeedingView{MaterialNo: "M1", Grams: "1", Time: base.Add(time.Minute), Registrar: "张三"},
			func(m feedingMismatch) bool { return m.time && !m.material && !m.grams && !m.registrar }, "投料时间"},
		{"登记人不同", FeedingView{MaterialNo: "M1", Grams: "1", Time: base, Registrar: "李四"},
			func(m feedingMismatch) bool { return m.registrar && !m.material && !m.grams && !m.time }, "登记人"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := feedingContentFromView(tc.view)
			if !ok {
				t.Fatalf("克数应可解析")
			}
			m := compareFeedingContent(got, ref)
			if !m.anyMismatch() {
				t.Fatalf("%s 不同应判为内容不符", tc.wantDim)
			}
			if !tc.wantFn(m) {
				t.Fatalf("只有 %s 维度应不同，得到 %+v", tc.wantDim, m)
			}
		})
	}

	// 数量合计相同不能抵消逐条差异：0.1+0.3 与 0.2+0.2 合计都是 0.4，
	// 但任意位置上的单条投料都不是同一项内容。
	x, _ := feedingContentFromView(FeedingView{MaterialNo: "M1", Grams: "0.1", Time: base, Registrar: "张三"})
	y, _ := feedingContentFromView(FeedingView{MaterialNo: "M1", Grams: "0.2", Time: base, Registrar: "张三"})
	if !compareFeedingContent(x, y).grams {
		t.Fatalf("克数 0.1 与 0.2 即使与另一条合计相同也应判为内容不符")
	}
}

// compareCloseFeedings 必须按登记顺序逐条对应：条数不符（含合并同物料的多次
// 投料）、调换位置、序号对不上、内容对不上都拒绝；克数非法单独成类；错误中
// 携带出错投料的位置。
func TestCompareCloseFeedingsRequiresOrderedPositionMatch(t *testing.T) {
	base := closeFeedTime()
	want := []feedingRecord{
		{Seq: 1, MaterialNo: "M1", GramsMilli: 100, Time: base.Add(2 * time.Hour), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", GramsMilli: 500, Time: base, Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", GramsMilli: 300, Time: base.Add(time.Hour), Registrar: "张三"},
	}
	viewOf := func(fs []FeedingView) []FeedingView { return fs }
	good := viewOf([]FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "0.100", Time: base.Add(2 * time.Hour).In(time.FixedZone("UTC+8", 8*3600)), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", Grams: "0.5", Time: base, Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", Grams: "0.300", Time: base.Add(time.Hour), Registrar: "张三"},
	})
	if err := compareCloseFeedings(good, want); err != nil {
		t.Fatalf("克数写法与时区写法不同但内容一致时应通过: %v", err)
	}

	// 把同物料（M1）的第 1、3 条合并成一条：条数不符，即使 0.4 = 0.1+0.3。
	merged := viewOf([]FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "0.4", Time: base.Add(2 * time.Hour), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", Grams: "0.5", Time: base, Registrar: "李四"},
	})
	if err := compareCloseFeedings(merged, want); err == nil {
		t.Fatalf("合并同物料的多次投料即使合计相同也应拒绝")
	}

	// 只交换同物料第 1、3 条的克数（位置与序号不变）：逐物料合计仍相等，
	// 但第 1 条内容对不上，必须报第 1 条而不是通过。
	swapped := viewOf([]FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "0.3", Time: base.Add(2 * time.Hour), Registrar: "张三"},
		{Seq: 2, MaterialNo: "M2", Grams: "0.5", Time: base, Registrar: "李四"},
		{Seq: 3, MaterialNo: "M1", Grams: "0.1", Time: base.Add(time.Hour), Registrar: "张三"},
	})
	err := compareCloseFeedings(swapped, want)
	var em *feedingEntryMismatch
	if !errors.As(err, &em) {
		t.Fatalf("同物料克数互换、合计相同应报内容不符（*feedingEntryMismatch），得到 %v", err)
	}
	if em.pos != 1 {
		t.Fatalf("应指出出错投料位置为第 1 条，得到第 %d 条", em.pos)
	}

	// 第 2 条序号被改：属于内容不符，位置为第 2 条。
	badSeq := viewOf([]FeedingView{
		good[0],
		{Seq: 9, MaterialNo: "M2", Grams: "0.5", Time: base, Registrar: "李四"},
		good[2],
	})
	err = compareCloseFeedings(badSeq, want)
	if !errors.As(err, &em) || em.pos != 2 {
		t.Fatalf("序号对不上应在第 2 条报内容不符，得到 %v", err)
	}

	// 第 2 条克数写法非法：单独报克数非法（*feedingGramError），与内容不符
	// 可区分，位置为第 2 条。
	badGrams := viewOf([]FeedingView{
		good[0],
		{Seq: 2, MaterialNo: "M2", Grams: "0.5x", Time: base, Registrar: "李四"},
		good[2],
	})
	var ge *feedingGramError
	if err = compareCloseFeedings(badGrams, want); !errors.As(err, &ge) {
		t.Fatalf("克数写法非法应报 *feedingGramError，得到 %v", err)
	}
	if ge.pos != 2 || ge.text != "0.5x" {
		t.Fatalf("克数非法应指出第 2 条与原文 %q，得到 pos=%d text=%q", "0.5x", ge.pos, ge.text)
	}
}

// 投料请求结果的克数被改成非法写法：Open 必须返回 ErrCorruptData，错误信息
// 指出请求编号、批次与“克数”，且这是克数非法而非泛化的内容不符；原文件
// 保持不变。
func TestOpenRejectsFeedingRequestResultIllegalGrams(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingRequestLedger(t, dir)
	bad := corruptFeedingRequestResult(t, good, func() json.RawMessage {
		return marshalFeedingView(t, FeedingView{
			Seq: 1, MaterialNo: "M1", Grams: "1.2x",
			Time: feedingRequestBaseTime(), Registrar: "张三",
		})
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("克数非法应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"feed-1", "B1", "克数", "1.2x"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
}

// 关闭结果第 2 条投料克数被改成非法写法：Open 返回 ErrCorruptData，错误信息
// 指出请求编号、批次、出错投料位置（第 2 条）与克数原文。
func TestOpenRejectsCloseRequestResultIllegalGramsWithPosition(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)
	bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
		return mutatedCloseView(t, good, func(v *BatchView) { v.Feedings[1].Grams = "0.5x" })
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("关闭结果克数非法应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"close-b1", "B1", "第 2 条", "克数", "0.5x"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
}

// 关闭结果把同物料第 1、3 条的克数互换（序号、位置、时间与登记人不变，逐
// 物料合计仍相等）：逐条对应不成立，Open 必须以 ErrCorruptData 拒绝并指出
// 出错位置为第 1 条——不能因数量合计相同而放行。
func TestOpenRejectsCloseRequestResultEqualTotalSwap(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)
	bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
		return mutatedCloseView(t, good, func(v *BatchView) {
			v.Feedings[0].Grams, v.Feedings[2].Grams = v.Feedings[2].Grams, v.Feedings[0].Grams
		})
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("合计相同但逐条内容不同应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"close-b1", "B1", "第 1 条", "不一致"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
}
