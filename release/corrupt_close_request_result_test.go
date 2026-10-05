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

// 本文件是“已保存的成功关闭请求的返回结果必须与原请求所指的已关闭批次一致”
// 的回归保障：批次用请求编号关闭后，按原编号、原内容再次提交 CloseBatch，
// 只能取回第一次关闭时确认的完整批次结果。台账里保存的关闭结果即使变成
// null、空对象，或被改成另一批次的结果、被改掉配方绑定/份数/投料/核对差额，
// 也不能被当作成功结果返回——打开台账及之后的每次查询/写入重载，都必须
// 核对保存结果与原请求所指批次（必须存在并仍为已关闭）的实际记录一致；
// 不一致按 ErrCorruptData 拒绝整份台账，不删除请求、不重做关闭，也不用
// 当前查询结果覆盖损坏结果。

// setupCloseRequestLedger 在 dir 建立一份正常台账并关闭：
//   - B1（已关闭）：RC/v1、3 份，按 wantCloseFeedings 登记三条投料
//     （f1、f2、f3），close-b1 关闭；
//   - B2（已关闭）：与 B1 相同的配方、份数与投料内容（f1-b2 等），
//     close-b2 关闭——其保存结果与 B1 的仅批次编号不同，用于验证
//     另一批次的结果不能顶替；
//   - B3（执行中）：RC/v1、2 份，无投料的正常批次，用于验证损坏的
//     关闭请求不能被“本次访问的是另一个正常批次”绕过。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupCloseRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCloseRecipe(t, s) // RC/v1：M1=0.1、M2=0.2
	if _, err := s.CreateBatch("b1", "B1", "RC", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	for i, want := range wantCloseFeedings() {
		reqNo := []string{"f1", "f2", "f3"}[i]
		if _, err := s.AddFeeding(reqNo, "B1", want.MaterialNo, want.Grams, want.Time, want.Registrar); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2：配方、份数、投料内容与 B1 完全相同，仅批次编号与请求编号不同。
	if _, err := s.CreateBatch("b2", "B2", "RC", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b2", "B2"); err != nil {
		t.Fatal(err)
	}
	for i, want := range wantCloseFeedings() {
		reqNo := []string{"f1-b2", "f2-b2", "f3-b2"}[i]
		if _, err := s.AddFeeding(reqNo, "B2", want.MaterialNo, want.Grams, want.Time, want.Registrar); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CloseBatch("close-b2", "B2"); err != nil {
		t.Fatal(err)
	}
	// B3：执行中、无投料的正常批次。
	if _, err := s.CreateBatch("b3", "B3", "RC", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b3", "B3"); err != nil {
		t.Fatal(err)
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

// closeResultRaw 取出正常台账中 reqNo 请求的保存结果原文。
func closeResultRaw(t *testing.T, good []byte, reqNo string) json.RawMessage {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[reqNo]
	if req == nil {
		t.Fatalf("正常台账中应存在 %s 请求记录", reqNo)
	}
	return req.Result
}

// mutatedCloseView 读出 close-b1 的保存结果，按 edit 修改后重新序列化。
func mutatedCloseView(t *testing.T, good []byte, edit func(*BatchView)) json.RawMessage {
	t.Helper()
	var v BatchView
	if err := json.Unmarshal(closeResultRaw(t, good, "close-b1"), &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// corruptCloseRequestResult 把正常台账内容中 close-b1 请求的保存结果替换为
// mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptCloseRequestResult(t *testing.T, good []byte, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["close-b1"]
	if req == nil {
		t.Fatalf("正常台账中应存在 close-b1 请求记录")
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 已保存的成功关闭请求的结果缺失、为 null、为空对象、无法读成批次结果，
// 或与原请求所指的已关闭批次不一致（批次编号、配方绑定、计划份数、状态、
// 投料条数/顺序/内容、逐物料应投量/实投量/差额有任何差异，含被另一批次的
// 结果顶替）时，Open 必须返回 ErrCorruptData：错误信息指出问题请求编号及
// 关联批次，不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptCloseRequestResult(t *testing.T) {
	base := closeFeedTime()
	cases := []struct {
		name   string
		mutate func(good []byte) json.RawMessage
	}{
		{"结果缺失", func(good []byte) json.RawMessage { return nil }},
		{"结果为 null", func(good []byte) json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func(good []byte) json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是批次结果对象", func(good []byte) json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果被替换成另一批次的关闭结果", func(good []byte) json.RawMessage {
			// B2 的配方、份数与投料数量和 B1 完全相同，但那是另一次关闭的结果。
			return closeResultRaw(t, good, "close-b2")
		}},
		{"结果批次编号被改成另一批次", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.BatchNo = "B2" })
		}},
		{"结果配方编号与批次绑定不符", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.RecipeNo = "RX" })
		}},
		{"结果配方版本与批次绑定不符", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.RecipeVersion = "v9" })
		}},
		{"结果配方名称与批次绑定不符", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.RecipeName = "被改的名称" })
		}},
		{"结果计划份数与批次不符", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.PlannedPortions = 4 })
		}},
		{"结果状态不是已关闭", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Status = StatusExecuting })
		}},
		{"投料少一条", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) {
				v.Feedings = v.Feedings[:len(v.Feedings)-1]
			})
		}},
		{"投料多一条", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) {
				v.Feedings = append(v.Feedings, FeedingView{
					Seq: 4, MaterialNo: "M1", Grams: "0.1", Time: base, Registrar: "张三",
				})
			})
		}},
		{"同物料的两次投料被合并成一条", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) {
				// 把第 1、3 条 M1 投料合并成一条 0.4，数量合计不变也不允许。
				v.Feedings = []FeedingView{
					{Seq: 1, MaterialNo: "M1", Grams: "0.4", Time: base.Add(2 * time.Hour), Registrar: "张三"},
					{Seq: 2, MaterialNo: "M2", Grams: "0.5", Time: base, Registrar: "李四"},
				}
			})
		}},
		{"投料登记顺序被调换", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) {
				v.Feedings[0], v.Feedings[1] = v.Feedings[1], v.Feedings[0]
			})
		}},
		{"投料序号被改", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Feedings[0].Seq = 5 })
		}},
		{"投料物料被改", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Feedings[0].MaterialNo = "M2" })
		}},
		{"投料克数被改", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Feedings[0].Grams = "0.2" })
		}},
		{"投料时间被改", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) {
				v.Feedings[0].Time = v.Feedings[0].Time.Add(time.Minute)
			})
		}},
		{"投料登记人被改", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Feedings[0].Registrar = "外人" })
		}},
		{"应投量被改坏", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Materials[0].RequiredGrams = "0.4" })
		}},
		{"实投量被改坏", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Materials[0].ActualGrams = "0.3" })
		}},
		{"只改坏核对差额而原始投料完整", func(good []byte) json.RawMessage {
			return mutatedCloseView(t, good, func(v *BatchView) { v.Materials[0].DifferenceGrams = "0" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupCloseRequestLedger(t, dir)
			bad := corruptCloseRequestResult(t, good, func() json.RawMessage { return tc.mutate(good) })
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存的关闭结果损坏应返回 ErrCorruptData，得到 %v", err)
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

// 原请求所指的批次不存在时，保存的关闭结果不能单独作为关闭成功的依据：
// 即使结果本身内容完整，也按损坏处理。
func TestOpenRejectsCloseRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 B1（close-b1 及 f1、f2、f3 对应的批次），保留完整的 B2、B3。
	kept := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B1" {
			kept = append(kept, b)
		}
	}
	st.Batches = kept
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("原批次不存在但保留关闭结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "B1") {
		t.Fatalf("错误信息应指出关联批次 B1，得到 %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 原请求所指的批次必须仍为已关闭：批次被改回执行中（投料原样保留）时，
// 保存的关闭结果与已确认记录不再一致，按损坏处理。
func TestOpenRejectsCloseRequestResultWhenBatchNotClosed(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	for _, b := range st.Batches {
		if b.BatchNo == "B1" {
			b.Status = StatusExecuting
		}
	}
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("批次不再是已关闭应返回 ErrCorruptData，得到 %v", err)
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
}

// 正常写法差异不能误判为损坏：保存结果的克数写成 0.100、应投量写成 0.300、
// 差额写成 -0.100，与批次记录是同一数量；投料时间表示同一时刻、仅时区写法
// 不同也合法。台账应正常打开，重放仍返回第一次关闭时的完整结果。
func TestOpenAcceptsCloseRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)

	bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
		return mutatedCloseView(t, good, func(v *BatchView) {
			v.Feedings[0].Grams = "0.100" // 与 0.1 是同一数量
			v.Feedings[0].Time = v.Feedings[0].Time.In(time.FixedZone("UTC+8", 8*3600))
			v.Materials[0].RequiredGrams = "0.300"
			v.Materials[1].ActualGrams = "0.500"
			v.Materials[1].DifferenceGrams = "-0.100"
		})
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("写法差异不应判为损坏: %v", err)
	}
	defer s.Close()

	// 重放取回保存的结果原文：第 1 条投料的克数保持 0.100 写法、时间保持
	// UTC+8 写法，但与批次实际投料是同一数量、同一时刻。
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	want := wantCloseFeedings()
	if replay.Feedings[0].Grams != "0.100" {
		t.Fatalf("重放应返回保存的克数写法 0.100，得到 %q", replay.Feedings[0].Grams)
	}
	if !replay.Feedings[0].Time.Equal(want[0].Time) {
		t.Fatalf("重放投料时间应与原投料为同一时刻，得到 %v", replay.Feedings[0].Time)
	}
	if replay.Materials[0].RequiredGrams != "0.300" || replay.Materials[1].DifferenceGrams != "-0.100" {
		t.Fatalf("重放应返回保存的核对项写法，得到 %+v", replay.Materials)
	}
	// 按批次查询得到系统规范化写法的视图，内容与首次关闭一致。
	checkClosedB1View(t, mustGetBatch(t, s, "B1"))
}

// 关闭只确认已有投料、不要求数量吻合：没有投料、实投为零、差额为负的批次
// 关闭后，保存结果如实记录零实投与负差额，不能被当成损坏。重新打开台账
// 应正常，重放仍返回第一次关闭时的完整结果。
func TestCloseRequestResultAllowsZeroActualAndNegativeDifference(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCloseRecipe(t, s)
	if _, err := s.CreateBatch("b9", "B9", "RC", "v1", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b9", "B9"); err != nil {
		t.Fatal(err)
	}
	first, err := s.CloseBatch("close-b9", "B9")
	if err != nil {
		t.Fatalf("无投料的执行中批次应可关闭: %v", err)
	}
	checkZeroClosed := func(v *BatchView) {
		t.Helper()
		if v.Status != StatusClosed || len(v.Feedings) != 0 {
			t.Fatalf("应为无投料的已关闭批次，得到 %+v", v)
		}
		mats := materialsMap(v)
		checkRequirement(t, mats, "M1", "0.3", "0", "-0.3")
		checkRequirement(t, mats, "M2", "0.6", "0", "-0.6")
	}
	checkZeroClosed(first)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("零实投与负差额不应判为损坏: %v", err)
	}
	defer s2.Close()
	replay, err := s2.CloseBatch("close-b9", "B9")
	if err != nil {
		t.Fatalf("重放关闭请求应成功: %v", err)
	}
	checkZeroClosed(replay)
	checkZeroClosed(mustGetBatch(t, s2, "B9"))
}

// 台账正常打开后，保存内容中的关闭请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一个正常批次，也不能忽略损坏的
// 关闭请求；被拒绝的重放不得返回损坏的保存结果，被拒绝的写入不留业务变化、
// 不占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，被拒绝过
// 的请求编号可以正常使用。
func TestCloseRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupCloseRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 close-b1 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptCloseRequestResult(t, good, func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B3"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原内容重放损坏的关闭请求：不得把损坏的保存结果当成成功
	// 结果返回，必须报损坏。
	if _, err := s.CloseBatch("close-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的关闭请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次的新写入同样不能绕过。
	if _, err := s.CloseBatch("close-b3", "B3"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上关闭正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("close-b3")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原成功请求仍取回第一次关闭的完整结果，被拒绝过的请求编号
	// 可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkClosedB1View(t, replay)
	closed, err := s.CloseBatch("close-b3", "B3")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if closed.Status != StatusClosed || closed.BatchNo != "B3" {
		t.Fatalf("B3 应被正常关闭，得到 %+v", closed)
	}
}
