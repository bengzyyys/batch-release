package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是“已保存的成功开始执行请求的返回结果必须对应原请求所指批次第一次
// 开始执行时的内容”的回归保障：批次用请求编号开始执行后，按原编号、原内容
// 再次提交 StartBatch，只能取回第一次开始时的结果（执行中、空投料、零实投、
// 差额为应投量的负值）。台账里保存的开始结果即使变成 null、空对象，或被改成
// 另一批次的结果、被改掉配方绑定/份数/状态，或混入了批次后来的投料与关闭
// 现状，也不能被当作成功结果返回——打开台账及之后的每次查询/写入重载，都
// 必须核对保存结果与原请求所指批次（必须存在且当前为执行中或已关闭）第一
// 次开始时的内容一致；不一致按 ErrCorruptData 拒绝整份台账，不删除请求、
// 不补造批次，也不用当前查询结果覆盖损坏结果。

// setupStartRequestLedger 在 dir 建立一份正常台账：
//   - B1（已关闭）：创建时用 R1/v1、5 份，草稿调整为 R1/v2、3 份后由
//     start-b1 开始，随后登记两条投料（f1、f2）并由 close-b1 关闭——
//     其保存的开始结果必须是首次开始时的内容（R1/v2、3 份、执行中、
//     空投料），不能混入后来的投料与关闭现状；
//   - B2（执行中）：与 B1 相同的最终配方与份数，start-b2 开始，无投料——
//     其保存结果与 B1 的首次开始结果仅批次编号不同，用于验证另一批次的
//     结果不能顶替；
//   - B3（执行中）：R1/v1、2 份，无投料的正常批次，用于验证损坏的开始
//     请求不能被“本次访问的是另一个正常批次”绕过。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupStartRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s) // R1/v1：M1=0.125、M2=0.5；R1/v2：M1=250、M4=2
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	base := replayFeedTime()
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", base, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f2", "B1", "M4", "2", base, "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2：最终配方与份数和 B1 相同，仅批次编号与请求编号不同。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b2", "B2"); err != nil {
		t.Fatal(err)
	}
	// B3：执行中、无投料的正常批次。
	if _, err := s.CreateBatch("b3", "B3", "R1", "v1", 2); err != nil {
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

// startResultRaw 取出正常台账中 reqNo 请求的保存结果原文。
func startResultRaw(t *testing.T, good []byte, reqNo string) json.RawMessage {
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

// mutatedStartView 读出 start-b1 的保存结果，按 edit 修改后重新序列化。
func mutatedStartView(t *testing.T, good []byte, edit func(*BatchView)) json.RawMessage {
	t.Helper()
	var v BatchView
	if err := json.Unmarshal(startResultRaw(t, good, "start-b1"), &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// corruptStartRequestResult 把正常台账内容中 start-b1 请求的保存结果替换为
// mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptStartRequestResult(t *testing.T, good []byte, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["start-b1"]
	if req == nil {
		t.Fatalf("正常台账中应存在 start-b1 请求记录")
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 已保存的成功开始执行请求的结果缺失、为 null、为空对象、无法读成批次结果，
// 或与原请求所指批次第一次开始时的内容不一致（批次编号、配方绑定、计划份数、
// 状态、投料列表、逐物料应投量/实投量/差额有任何差异，含被另一批次的结果
// 顶替、混入后来的投料或关闭现状）时，Open 必须返回 ErrCorruptData：错误
// 信息指出问题请求编号及关联批次，不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptStartRequestResult(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(good []byte) json.RawMessage
	}{
		{"结果缺失", func(good []byte) json.RawMessage { return nil }},
		{"结果为 null", func(good []byte) json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func(good []byte) json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是批次结果对象", func(good []byte) json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果被替换成另一批次的开始结果", func(good []byte) json.RawMessage {
			// B2 的最终配方、份数与 B1 相同，但那是另一批次的开始结果。
			return startResultRaw(t, good, "start-b2")
		}},
		{"结果批次编号被改成另一批次", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.BatchNo = "B2" })
		}},
		{"结果配方编号与批次绑定不符", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.RecipeNo = "RX" })
		}},
		{"结果沿用创建草稿时的旧版本", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.RecipeVersion = "v1" })
		}},
		{"结果配方名称与批次绑定不符", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.RecipeName = "被改的名称" })
		}},
		{"结果沿用创建草稿时的旧份数", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.PlannedPortions = 5 })
		}},
		{"结果状态被改成关闭后的现状", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Status = StatusClosed })
		}},
		{"结果混入了后来追加的投料", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				base := replayFeedTime()
				v.Feedings = []FeedingView{
					{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: base, Registrar: "张三"},
					{Seq: 2, MaterialNo: "M4", Grams: "2", Time: base, Registrar: "李四"},
				}
			})
		}},
		{"物料核对项少一项", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				v.Materials = v.Materials[:1]
			})
		}},
		{"物料核对项混入旧版本的物料", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				v.Materials = append(v.Materials, MaterialRequirement{
					MaterialNo: "M2", RequiredGrams: "1.5", ActualGrams: "0", DifferenceGrams: "-1.5",
				})
			})
		}},
		{"物料核对顺序被调换", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				v.Materials[0], v.Materials[1] = v.Materials[1], v.Materials[0]
			})
		}},
		{"应投量被改成旧计划的值", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].RequiredGrams = "0.625" })
		}},
		{"实投量被改成非零", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].ActualGrams = "100.5" })
		}},
		{"差额被改坏", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].DifferenceGrams = "0" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupStartRequestLedger(t, dir)
			bad := corruptStartRequestResult(t, good, func() json.RawMessage { return tc.mutate(good) })
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存的开始结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"start-b1", "B1"} {
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

// 原请求所指的批次不存在时，保存的开始结果不能单独作为开始成功的依据：
// 即使结果本身内容完整，也按损坏处理。
func TestOpenRejectsStartRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupStartRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 B1（start-b1、close-b1 及 f1、f2 对应的批次），保留完整的 B2、B3。
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
		t.Fatalf("原批次不存在但保留开始结果应返回 ErrCorruptData，得到 %v", err)
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

// 原请求所指的批次当前只能是执行中或已关闭：批次被改回草稿时，保存的开始
// 结果与已确认记录不再一致，按损坏处理。
func TestOpenRejectsStartRequestResultWhenBatchBackToDraft(t *testing.T) {
	dir := t.TempDir()
	good := setupStartRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	for _, b := range st.Batches {
		if b.BatchNo == "B1" {
			b.Status = StatusDraft
			b.Feedings = nil // 草稿带投料本身即损坏，这里只针对状态回退
		}
	}
	// 批次被改回草稿后，后来的投料与关闭请求记录也随之失去对应记录，
	// 一并移除，让开始请求的状态回退成为唯一的损坏点。
	delete(st.Requests, "f1")
	delete(st.Requests, "f2")
	delete(st.Requests, "close-b1")
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("批次被改回草稿应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"start-b1", "B1"} {
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

// 正常写法差异不能误判为损坏：保存结果的应投量写成 750.000、实投量写成
// 0.000、差额写成 -750.000，与首次开始时的数量是同一数值；合法的零实投与
// 负差额不是损坏。台账应正常打开，重放仍返回保存的写法。
func TestOpenAcceptsStartRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupStartRequestLedger(t, dir)

	bad := corruptStartRequestResult(t, good, func() json.RawMessage {
		return mutatedStartView(t, good, func(v *BatchView) {
			v.Materials[0].RequiredGrams = "750.000" // 与 750 是同一数量
			v.Materials[0].ActualGrams = "0.000"     // 与 0 是同一数量
			v.Materials[0].DifferenceGrams = "-750.000"
			v.Materials[1].RequiredGrams = "6.000"
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

	// 重放取回保存的结果原文：核对项保持 750.000、0.000、-750.000 写法，
	// 但与首次开始时的数量是同一数值。
	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重放开始请求应成功: %v", err)
	}
	if replay.Materials[0].RequiredGrams != "750.000" ||
		replay.Materials[0].ActualGrams != "0.000" ||
		replay.Materials[0].DifferenceGrams != "-750.000" {
		t.Fatalf("重放应返回保存的核对项写法，得到 %+v", replay.Materials[0])
	}
	// 批次后来的投料与关闭属于现状：查询显示已关闭与当前投料，不影响
	// 首次开始结果的合法性。
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))
}

// checkClosedCurrentAfterSetup 校验 setupStartRequestLedger 中 B1 的当前
// 台账记录：已关闭、R1/v2、3 份，保留 f1、f2 两条投料。
func checkClosedCurrentAfterSetup(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" || v.Status != StatusClosed || v.PlannedPortions != 3 ||
		v.RecipeNo != "R1" || v.RecipeVersion != "v2" {
		t.Fatalf("当前台账应为已关闭的 R1/v2、3 份，得到 %+v", v)
	}
	if len(v.Feedings) != 2 {
		t.Fatalf("当前台账应保留 2 条投料，得到 %d 条", len(v.Feedings))
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "M1", "750", "100.5", "-649.5")
	checkRequirement(t, mats, "M4", "6", "2", "-4")
}

// 批次后来追加的投料与关闭状态属于批次现状，不能混入第一次开始的结果，
// 也不能成为拒绝合法旧结果的理由：台账中 B1 已投料并关闭，其保存的开始
// 结果仍是首次开始时的内容（执行中、空投料、零实投），打开与重放都必须
// 正常。
func TestStartRequestResultStaysFirstStartAfterFeedingAndClose(t *testing.T) {
	dir := t.TempDir()
	setupStartRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("已投料并关闭的批次不应让其首次开始结果被判损坏: %v", err)
	}
	defer s.Close()

	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replay)
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))
}

// 台账正常打开后，保存内容中的开始请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一个正常批次，也不能忽略损坏的
// 开始请求；被拒绝的重放不得返回损坏的保存结果，被拒绝的写入不留业务变化、
// 不占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，被拒绝过
// 的请求编号可以正常使用。
func TestStartRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupStartRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 start-b1 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptStartRequestResult(t, good, func() json.RawMessage {
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
	// 用原编号、原内容重放损坏的开始请求：不得把损坏的保存结果当成成功
	// 结果返回，必须报损坏。
	if _, err := s.StartBatch("start-b1", "B1"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的开始请求应返回 ErrCorruptData，得到 %v", err)
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

	// 恢复后：原成功请求仍取回第一次开始时的结果，被拒绝过的请求编号
	// 可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkFirstStartView(t, replay)
	closed, err := s.CloseBatch("close-b3", "B3")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if closed.Status != StatusClosed || closed.BatchNo != "B3" {
		t.Fatalf("B3 应被正常关闭，得到 %+v", closed)
	}
}
