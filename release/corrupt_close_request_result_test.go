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

// 本文件是“已保存的成功关闭批次请求的返回结果必须与已关闭批次一致”的
// 回归保障：执行中的批次用请求编号关闭后，按原编号、原内容再次提交，
// 只能取回第一次确认的完整批次结果。台账里保存的关闭结果即使变成 null、
// 空对象，或被替换成缺少批次/投料/核对信息的结果，也不能被当作成功结果
// 返回——打开台账及之后的每次查询/写入重载，都必须核对保存结果与原请求
// 所指已关闭批次的实际内容（配方绑定、计划份数、全部投料及登记顺序、
// 逐物料应投/实投/差额）一致；不一致按 ErrCorruptData 拒绝整份台账，
// 不删除请求、不重做关闭，也不用当前查询结果覆盖损坏结果。

// setupCloseRequestLedger 在 dir 建立一份正常台账：
//   - RC/v1（双料配方）：M1=0.1、M2=0.2；
//   - B1：3 份，执行后登记三条投料（见 wantCloseFeedings），用 "close-b1"
//     关闭——M1 超投 0.1、M2 欠投 0.1，关闭结果含一正一负真实差额；
//   - B2：RC/v1、5 份的草稿，不含关闭请求，用于验证整份台账核对与本次
//     访问的是哪个批次无关。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupCloseRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCloseRecipe(t, s)
	if _, err := s.CreateBatch("b1", "B1", "RC", "v1", 3); err != nil {
		t.Fatalf("创建 B1 失败: %v", err)
	}
	// B2：RC/v1、5 份的草稿，不含关闭请求，用于验证整份台账核对与本次
	// 访问的是哪个批次无关。
	if _, err := s.CreateBatch("b2", "B2", "RC", "v1", 5); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	for i, want := range wantCloseFeedings() {
		reqNo := []string{"f1", "f2", "f3"}[i]
		if _, err := s.AddFeeding(reqNo, "B1", want.MaterialNo, want.Grams, want.Time, want.Registrar); err != nil {
			t.Fatalf("投料 %s 失败: %v", reqNo, err)
		}
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatalf("关闭 B1 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return good
}

// marshalLedgerState 把台账状态重新序列化（缩进格式，便于写入文件）。
func marshalLedgerState(t *testing.T, st *persistedState) []byte {
	t.Helper()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// setCloseResultRaw 把 close-b1 请求的保存结果整体替换为 raw
// （nil 表示删除该字段），返回改写后的文件内容。
func setCloseResultRaw(t *testing.T, good []byte, raw json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	st.Requests["close-b1"].Result = raw
	return marshalLedgerState(t, &st)
}

// editCloseResult 解出 close-b1 保存的批次结果，交给 fn 改坏后再写回。
func editCloseResult(t *testing.T, good []byte, fn func(*BatchView)) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	var v BatchView
	if err := json.Unmarshal(st.Requests["close-b1"].Result, &v); err != nil {
		t.Fatal(err)
	}
	fn(&v)
	raw, err := json.Marshal(&v)
	if err != nil {
		t.Fatal(err)
	}
	st.Requests["close-b1"].Result = raw
	return marshalLedgerState(t, &st)
}

// writeLedger 把改坏后的内容写回台账文件。
func writeLedger(t *testing.T, dir string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 已保存的成功关闭请求结果缺失、为 null、为空对象、无法读成批次结果，
// 或与原请求所指已关闭批次的任一部分不一致时，Open 必须返回
// ErrCorruptData：错误信息指出问题请求编号 close-b1 及关联批次 B1，
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptCloseRequestResult(t *testing.T) {
	base := closeFeedTime()
	cases := []struct {
		name   string
		mutate func(*testing.T, []byte) []byte
	}{
		{"结果缺失", func(t *testing.T, good []byte) []byte {
			return setCloseResultRaw(t, good, nil)
		}},
		{"结果为 null", func(t *testing.T, good []byte) []byte {
			return setCloseResultRaw(t, good, json.RawMessage("null"))
		}},
		{"结果为空对象", func(t *testing.T, good []byte) []byte {
			return setCloseResultRaw(t, good, json.RawMessage(`{}`))
		}},
		{"结果只有批次编号", func(t *testing.T, good []byte) []byte {
			return setCloseResultRaw(t, good, json.RawMessage(`{"BatchNo":"B1"}`))
		}},
		{"结果不是批次对象", func(t *testing.T, good []byte) []byte {
			return setCloseResultRaw(t, good, json.RawMessage(`"not-an-object"`))
		}},
		{"结果是数组而不是批次对象", func(t *testing.T, good []byte) []byte {
			return setCloseResultRaw(t, good, json.RawMessage(`[]`))
		}},
		{"结果批次编号被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.BatchNo = "B9" })
		}},
		{"结果配方编号与实际绑定不符", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.RecipeNo = "RX" })
		}},
		{"结果配方版本与实际绑定不符", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.RecipeVersion = "v9" })
		}},
		{"结果配方名称与实际绑定不符", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.RecipeName = "被改名的配方" })
		}},
		{"结果计划份数与实际不符", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.PlannedPortions = 4 })
		}},
		{"结果状态不是已关闭", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Status = StatusExecuting })
		}},
		{"结果少一条投料", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) {
				v.Feedings = v.Feedings[:2]
			})
		}},
		{"结果多一条投料", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) {
				v.Feedings = append(v.Feedings, FeedingView{
					Seq: 4, MaterialNo: "M1", Grams: "1", Time: base, Registrar: "外人",
				})
			})
		}},
		{"结果合并同物料的两次投料", func(t *testing.T, good []byte) []byte {
			// 第 1、3 条都是 M1（0.1+0.3），合并成一条 0.4 会少一条记录，
			// 即使累计实投相同也不能接受。
			return editCloseResult(t, good, func(v *BatchView) {
				merged := append([]FeedingView{}, v.Feedings[:2]...)
				merged[0].Grams = "0.4"
				v.Feedings = merged
			})
		}},
		{"结果投料序号被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Feedings[1].Seq = 9 })
		}},
		{"结果投料物料被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Feedings[0].MaterialNo = "M2" })
		}},
		{"结果投料数量被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Feedings[0].Grams = "0.2" })
		}},
		{"结果投料时间被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) {
				v.Feedings[0].Time = v.Feedings[0].Time.Add(time.Minute)
			})
		}},
		{"结果投料登记人被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Feedings[0].Registrar = "外人" })
		}},
		{"结果投料顺序被调换", func(t *testing.T, good []byte) []byte {
			// 只调换前两条的位置却保留原序号：位置与序号不再对应。
			return editCloseResult(t, good, func(v *BatchView) {
				v.Feedings[0], v.Feedings[1] = v.Feedings[1], v.Feedings[0]
			})
		}},
		{"结果投料列表为 null", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Feedings = nil })
		}},
		{"结果少一项物料核对", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Materials = v.Materials[:1] })
		}},
		{"结果多一项物料核对", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) {
				v.Materials = append(v.Materials, MaterialRequirement{MaterialNo: "FAKE"})
			})
		}},
		{"结果核对物料编号被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Materials[0].MaterialNo = "M9" })
		}},
		{"结果应投量被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Materials[0].RequiredGrams = "0.9" })
		}},
		{"结果实投量被改", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Materials[0].ActualGrams = "0.1" })
		}},
		{"只改坏核对差额而投料完整", func(t *testing.T, good []byte) []byte {
			// 原始投料一条未动，仅把 M1 的差额改成零：仍属于结果与已确认
			// 记录不一致，必须拒绝。
			return editCloseResult(t, good, func(v *BatchView) { v.Materials[0].DifferenceGrams = "0" })
		}},
		{"结果负差额被改成零", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Materials[1].DifferenceGrams = "0" })
		}},
		{"结果核对列表为 null", func(t *testing.T, good []byte) []byte {
			return editCloseResult(t, good, func(v *BatchView) { v.Materials = nil })
		}},
		{"结果被另一批次的结果改贴编号后顶替", func(t *testing.T, good []byte) []byte {
			// 另建 B2：RC/v1、3 份，只登记一条 M1 0.4 的投料并关闭，
			// 配方与 M1 累计数量都和 B1 相同，但投料条数与 M2 情况不同。
			// 把 B2 的关闭结果改贴 B1 编号后顶替 close-b1 的结果。
			return closeResultFromOtherBatch(t, good, true)
		}},
		{"结果被另一批次的结果原样顶替", func(t *testing.T, good []byte) []byte {
			// B2 的关闭结果原样顶替，保存结果的批次编号仍是 B2：
			// 与原请求所指的 B1 不一致，不能当作 B1 的关闭结果。
			return closeResultFromOtherBatch(t, good, false)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupCloseRequestLedger(t, dir)
			bad := tc.mutate(t, good)
			writeLedger(t, dir, bad)

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("关闭结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"close-b1", "B1"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
				}
			}
			got, err := os.ReadFile(filepath.Join(dir, stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, bad) {
				t.Fatalf("拒绝打开不应改动台账文件")
			}
		})
	}
}

// closeResultFromOtherBatch 在一份含已关闭 B2 的台账中，取 B2 的关闭结果
// 顶替 close-b1 的结果：relabel 为 true 时把批次编号改贴成 B1，false 时
// 原样顶替（保存结果的批次编号仍是 B2）。配方编号、版本、M1 累计数量
// 相同也不能放行。
func closeResultFromOtherBatch(t *testing.T, good []byte, relabel bool) []byte {
	t.Helper()
	dir2 := t.TempDir()
	s, err := Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	registerCloseRecipe(t, s) // RC/v1，与 B1 相同配方
	if _, err := s.CreateBatch("b2", "B2", "RC", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b2", "B2"); err != nil {
		t.Fatal(err)
	}
	base := closeFeedTime()
	if _, err := s.AddFeeding("g1", "B2", "M1", "0.4", base, "张三"); err != nil {
		t.Fatal(err)
	}
	b2View, err := s.CloseBatch("close-b2", "B2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if relabel {
		// 改贴批次编号，伪装成 B1 的关闭结果。
		b2View.BatchNo = "B1"
	}
	raw, err := json.Marshal(b2View)
	if err != nil {
		t.Fatal(err)
	}
	return setCloseResultRaw(t, good, raw)
}

// 原请求对应的批次不存在时，保存的关闭结果不能单独作为关闭成功的依据：
// 即使结果本身字段完整，也按损坏处理。为直接命中关闭请求核对分支，
// 同时删除该批次的投料请求记录。
func TestOpenRejectsCloseRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	kept := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B1" {
			kept = append(kept, b)
		}
	}
	st.Batches = kept
	for _, reqNo := range []string{"f1", "f2", "f3"} {
		delete(st.Requests, reqNo)
	}
	bad := marshalLedgerState(t, &st)
	writeLedger(t, dir, bad)

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("原批次不存在但保留关闭结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	if !strings.Contains(msg, "close-b1") || !strings.Contains(msg, "B1") {
		t.Fatalf("错误信息应指出请求 close-b1 与批次 B1，得到 %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 原请求对应的批次不再是已关闭（被改回执行中）时，保存的关闭结果与批次
// 状态不一致，按损坏处理——不能重做关闭，也不能把保存结果当真。
func TestOpenRejectsCloseRequestResultWhenBatchNoLongerClosed(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	b := findBatch(&st, "B1")
	if b == nil {
		t.Fatalf("正常台账中应存在 B1")
	}
	b.Status = StatusExecuting
	bad := marshalLedgerState(t, &st)
	writeLedger(t, dir, bad)

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("批次不再是已关闭但保留关闭结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	if !strings.Contains(msg, "close-b1") || !strings.Contains(msg, "B1") {
		t.Fatalf("错误信息应指出请求 close-b1 与批次 B1，得到 %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 正常写法差异不能误判为损坏：投料时间是同一时刻的另一种时区写法、核对
// 数量写成三位小数（0.300 与 0.3）甚至带正号（+0.1）都与系统保存的结果
// 表示同一内容。台账应正常打开，重放仍返回第一次关闭的完整结果。
func TestOpenAcceptsCloseRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)

	bad := editCloseResult(t, good, func(v *BatchView) {
		// 投料时间改成同一时刻的 UTC+8 写法（时刻不变）。
		tz8 := time.FixedZone("UTC+8", 8*3600)
		for i := range v.Feedings {
			v.Feedings[i].Time = v.Feedings[i].Time.In(tz8)
		}
		// 逐物料核对改成等值的不同写法（含正号、补零与三位小数）。
		v.Materials[0].RequiredGrams = "0.300"
		v.Materials[0].ActualGrams = "0.400"
		v.Materials[0].DifferenceGrams = "+0.100"
		v.Materials[1].RequiredGrams = "0.600"
		v.Materials[1].ActualGrams = "0.500"
		v.Materials[1].DifferenceGrams = "-0.100"
	})
	writeLedger(t, dir, bad)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("同一时刻与等值写法差异不应判为损坏: %v", err)
	}
	defer s.Close()

	// 重放取回第一次关闭保存的完整结果。保存结果中的克数按原文保留
	// （0.300、+0.100 等写法原样返回，与投料请求结果保留 "1" 的既有语义
	// 一致），因此这里按精确数值与同一时刻做语义比较，不要求规范字符串。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放应成功: %v", err)
	}
	checkClosedB1Semantic(t, replay)
	// 按批次查询由实际记录重新构造，显示规范写法。
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))
}

// checkClosedB1Semantic 与 checkClosedB1View 内容相同，但克数按精确数值、
// 时间按同一时刻比较，用于重放保留保存结果原文写法的场景。
func checkClosedB1Semantic(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" || v.RecipeNo != "RC" || v.RecipeVersion != "v1" ||
		v.RecipeName != "双料配方" || v.PlannedPortions != 3 || v.Status != StatusClosed {
		t.Fatalf("批次绑定、份数或状态不正确: %+v", v)
	}
	wantFeedings := wantCloseFeedings()
	if len(v.Feedings) != len(wantFeedings) {
		t.Fatalf("应保留 %d 条投料，得到 %d 条", len(wantFeedings), len(v.Feedings))
	}
	for i, want := range wantFeedings {
		got := v.Feedings[i]
		gotMilli, err := parseGrams(got.Grams)
		if err != nil {
			t.Fatalf("第 %d 条投料克数 %q 不合法: %v", i+1, got.Grams, err)
		}
		wantMilli, _ := parseGrams(want.Grams)
		if got.Seq != want.Seq || got.MaterialNo != want.MaterialNo ||
			gotMilli != wantMilli || !got.Time.Equal(want.Time) ||
			got.Registrar != want.Registrar {
			t.Fatalf("第 %d 条投料语义上应为 %+v，得到 %+v", i+1, want, got)
		}
	}
	mats := materialsMap(v)
	checkRequirementMilli(t, mats, "M1", "0.3", "0.4", "0.1")
	checkRequirementMilli(t, mats, "M2", "0.6", "0.5", "-0.1")
}

// checkRequirementMilli 与 checkRequirement 相同，但按精确数值比较，
// 允许保存结果保留 0.300、+0.100 等等值写法。
func checkRequirementMilli(t *testing.T, got map[string]MaterialRequirement, mat, req, act, diff string) {
	t.Helper()
	m, ok := got[mat]
	if !ok {
		t.Fatalf("数量核对缺少物料 %q，实际有 %+v", mat, got)
	}
	wantReq, _ := parseGramsSigned(req)
	wantAct, _ := parseGramsSigned(act)
	wantDiff, _ := parseGramsSigned(diff)
	gotReq, err := parseGramsSigned(m.RequiredGrams)
	if err != nil {
		t.Fatalf("物料 %q 应投量 %q 不合法", mat, m.RequiredGrams)
	}
	gotAct, err := parseGramsSigned(m.ActualGrams)
	if err != nil {
		t.Fatalf("物料 %q 实投量 %q 不合法", mat, m.ActualGrams)
	}
	gotDiff, err := parseGramsSigned(m.DifferenceGrams)
	if err != nil {
		t.Fatalf("物料 %q 差额 %q 不合法", mat, m.DifferenceGrams)
	}
	if gotReq != wantReq || gotAct != wantAct || gotDiff != wantDiff {
		t.Fatalf("物料 %q 核对数值应为 应投=%s 实投=%s 差额=%s，得到 应投=%s 实投=%s 差额=%s",
			mat, req, act, diff, m.RequiredGrams, m.ActualGrams, m.DifferenceGrams)
	}
}

// 投料克数的写法差异也按精确数值核对：保存结果把 0.1 写成 0.100 是同一
// 数量，台账正常打开、批次查询显示规范写法的实际投料。
func TestOpenAcceptsCloseRequestFeedingGramsVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)
	bad := editCloseResult(t, good, func(v *BatchView) {
		v.Feedings[0].Grams = "0.100"
		v.Feedings[1].Grams = "0.500"
		v.Feedings[2].Grams = "0.300"
	})
	writeLedger(t, dir, bad)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("克数等值写法不应判为损坏: %v", err)
	}
	defer s.Close()

	// 批次查询由实际投料记录构造，仍显示规范写法与登记顺序。
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))
}

// 关闭不要求数量吻合：无投料批次的真实零实投与负差额不是损坏。重新打开
// 后用原关闭请求编号重放，仍返回第一次关闭时的完整结果。
func TestCloseRequestResultWithZeroActualAndNegativeDifference(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCloseRecipe(t, s)
	if _, err := s.CreateBatch("b1", "B1", "RC", "v1", 3); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	first, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("无投料批次应可关闭: %v", err)
	}
	if len(first.Feedings) != 0 {
		t.Fatalf("首次关闭结果应无投料，得到 %d 条", len(first.Feedings))
	}
	mats := materialsMap(first)
	checkRequirement(t, mats, "M1", "0.3", "0", "-0.3")
	checkRequirement(t, mats, "M2", "0.6", "0", "-0.6")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("含零实投与负差额关闭结果的台账应能重新打开: %v", err)
	}
	defer s2.Close()
	replay, err := s2.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重新打开后重放应成功: %v", err)
	}
	if len(replay.Feedings) != 0 {
		t.Fatalf("重放结果应无投料，得到 %d 条", len(replay.Feedings))
	}
	replayMats := materialsMap(replay)
	checkRequirement(t, replayMats, "M1", "0.3", "0", "-0.3")
	checkRequirement(t, replayMats, "M2", "0.6", "0", "-0.6")
}

// 台账正常打开后，保存内容中的关闭请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一个正常批次 B2，也不能忽略
// 损坏的关闭请求；被拒绝的重放不得返回损坏结果，被拒绝的写入不留业务
// 变化、不占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，
// 被拒绝过的请求编号可以正常使用。
func TestCloseRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer s.Close()

	// 打开后把 close-b1 的保存结果改坏为空对象（B2 等其余记录完整）。
	bad := setCloseResultRaw(t, good, json.RawMessage(`{}`))
	writeLedger(t, dir, bad)

	// 查询损坏批次与正常批次 B2 都必须失败，不能绕过对全部关闭请求的核对。
	for _, batchNo := range []string{"B1", "B2"} {
		v, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原批次重放损坏的关闭请求：不得把空对象当成成功结果。
	if v, err := s.CloseBatch("close-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的关闭请求应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的重放不应返回批次视图: %+v", v)
	}
	// 对正常批次 B2 的新写入同样不能绕过核对。
	if _, err := s.StartBatch("start-b2", "B2"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上写入正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("start-b2")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原关闭请求仍取回第一次确认的完整结果，B1 内容保持原样，
	// 被拒绝过的请求编号 start-b2 可以正常使用（没有被占用）。
	writeLedger(t, dir, good)
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("恢复后重放原关闭请求失败: %v", err)
	}
	checkClosedB1View(t, replay)
	b2, err := s.StartBatch("start-b2", "B2")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if b2.Status != StatusExecuting {
		t.Fatalf("B2 应已开始执行，得到状态 %s", b2.Status)
	}
}

// 读取核对不改变原有的请求语义：
//   - 换新编号关闭已关闭批次仍按状态限制返回 ErrInvalidState；
//   - 首次关闭编号用于另一批次仍返回 ErrRequestConflict；
//   - 同编号同批次重复关闭始终返回首次完整结果（重新打开后亦然）。
func TestCloseRequestCheckKeepsStateAndConflictSemantics(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	first := setupClosedBatch(t, s)
	checkClosedB1View(t, first)

	if _, err := s.CloseBatch("close-again", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("新编号关闭已关闭批次应返回 ErrInvalidState，得到 %v", err)
	}
	if _, err := s.CreateBatch("b2", "B2", "RC", "v1", 2); err != nil {
		t.Fatalf("创建 B2 失败: %v", err)
	}
	if _, err := s.CloseBatch("close-b1", "B2"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同编号关闭另一批次应返回 ErrRequestConflict，得到 %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	defer s2.Close()
	replay, err := s2.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重新打开后同编号同批次重放应成功: %v", err)
	}
	checkClosedB1View(t, replay)
	if _, err := s2.CloseBatch("close-again", "B1"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("重新打开后新编号关闭已关闭批次仍应返回 ErrInvalidState，得到 %v", err)
	}
}
