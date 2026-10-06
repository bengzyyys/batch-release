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

// 本文件是“已保存的成功开始执行请求的返回结果必须是原请求所指批次第一次
// 开始执行时的内容”的回归保障：批次用请求编号开始执行后，按原编号、原内容
// 再次提交 StartBatch，只能取回第一次开始执行那一刻的批次结果。台账里保存
// 的开始结果即使变成 null、空对象，或被改成另一批次的结果、混入批次后来的
// 投料与关闭状态、被改掉开始时固定的配方绑定/计划份数/核对数量，也不能被
// 当作成功结果返回——打开台账及之后的每次查询/写入重载，都必须核对保存结果
// 与原请求所指批次（必须存在且当前为执行中或已关闭）第一次开始时的记录一致；
// 不一致按 ErrCorruptData 拒绝整份台账，不删除请求、不补造批次，也不用当前
// 查询结果覆盖损坏结果。

// setupStartRequestLedger 在 dir 建立一份正常台账：
//   - B1（已关闭）：创建时先用 R1/v1、5 份，草稿调整为 R1/v2、3 份后用
//     start-b1 开始执行（第一次开始结果只含最终计划），随后登记 f1、f2、f3
//     三条投料并用 close-b1 关闭——用于验证后来的投料与关闭状态不能混入
//     第一次开始的结果，也不能因结果与批次现状不同而拒绝合法旧结果；
//   - B2（执行中）：R1/v2、3 份、无投料，用 start-b2 开始——其保存结果与
//     B1 第一次开始的结果仅批次编号不同，用于验证另一批次的结果不能顶替；
//   - B3（执行中）：R1/v1、7 份、无投料的正常批次，用于验证损坏的开始
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

	// B1：草稿先选旧计划 R1/v1、5 份，最后调整为 R1/v2、3 份再开始。
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
	if _, err := s.AddFeeding("f2", "B1", "M4", "2", base.Add(time.Hour), "李四"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f3", "B1", "M1", "50", base.Add(2*time.Hour), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}

	// B2：最终计划与 B1 开始时完全相同，仅批次编号与请求编号不同。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b2", "B2"); err != nil {
		t.Fatal(err)
	}

	// B3：执行中、无投料的正常批次（旧版本、7 份）。
	if _, err := s.CreateBatch("b3", "B3", "R1", "v1", 7); err != nil {
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
// 或与原请求所指批次第一次开始执行时的记录不一致（批次编号、开始时固定的
// 配方绑定/计划份数、执行中状态、空投料列表、逐物料应投量/零实投量/负差额
// 有任何差异，含被另一批次的结果顶替、混入草稿旧计划或批次后来的投料与关闭
// 状态）时，Open 必须返回 ErrCorruptData：错误信息指出问题请求编号及关联
// 批次，不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptStartRequestResult(t *testing.T) {
	base := replayFeedTime()
	cases := []struct {
		name   string
		mutate func(good []byte) json.RawMessage
	}{
		{"结果缺失", func(good []byte) json.RawMessage { return nil }},
		{"结果为 null", func(good []byte) json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func(good []byte) json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是批次结果对象", func(good []byte) json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果被替换成另一批次的开始结果", func(good []byte) json.RawMessage {
			// B2 开始时的配方、份数与 B1 完全相同，但那是另一次开始的结果。
			return startResultRaw(t, good, "start-b2")
		}},
		{"结果批次编号被改成另一批次", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.BatchNo = "B2" })
		}},
		{"结果配方编号与开始时固定的绑定不符", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.RecipeNo = "RX" })
		}},
		{"结果配方版本被改成同配方的其他版本", func(good []byte) json.RawMessage {
			// 草稿曾用过 R1/v1，但最后选定的是 v2；其他版本不能顶替。
			return mutatedStartView(t, good, func(v *BatchView) { v.RecipeVersion = "v1" })
		}},
		{"结果配方名称与开始时固定的绑定不符", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.RecipeName = "被改的名称" })
		}},
		{"结果计划份数退回草稿调整前的旧计划", func(good []byte) json.RawMessage {
			// 创建草稿时是 5 份，最后调整为 3 份；必须以最后选定的计划为准。
			return mutatedStartView(t, good, func(v *BatchView) { v.PlannedPortions = 5 })
		}},
		{"结果状态被混入批次现状的已关闭", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Status = StatusClosed })
		}},
		{"结果状态被改成草稿", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Status = StatusDraft })
		}},
		{"结果混入批次后来追加的投料", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				v.Feedings = []FeedingView{
					{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: base, Registrar: "张三"},
				}
			})
		}},
		{"物料核对项少一项", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				v.Materials = v.Materials[:1]
			})
		}},
		{"物料核对项多一项", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				v.Materials = append(v.Materials, MaterialRequirement{
					MaterialNo: "M2", RequiredGrams: "2.5", ActualGrams: "0", DifferenceGrams: "-2.5",
				})
			})
		}},
		{"物料排列顺序被调换", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) {
				v.Materials[0], v.Materials[1] = v.Materials[1], v.Materials[0]
			})
		}},
		{"物料编号被改", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].MaterialNo = "M2" })
		}},
		{"应投量被改坏", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].RequiredGrams = "125.5" })
		}},
		{"实投量混入后来的累计投料", func(good []byte) json.RawMessage {
			// 第一次开始时实投必须为零；150.5 是 B1 关闭前 M1 的累计实投。
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].ActualGrams = "150.5" })
		}},
		{"只改坏核对差额而其余内容完整", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].DifferenceGrams = "0" })
		}},
		{"差额没有取应投量的负值", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[1].DifferenceGrams = "6" })
		}},
		{"核对数量写成无法解析的字符串", func(good []byte) json.RawMessage {
			return mutatedStartView(t, good, func(v *BatchView) { v.Materials[0].RequiredGrams = "abc" })
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

// 开始请求保存的提交内容本身无法解析时，同样按损坏处理：无法重放回第一次
// 成功时的结果，错误信息至少指出问题请求编号。
func TestOpenRejectsCorruptStartRequestPayload(t *testing.T) {
	dir := t.TempDir()
	good := setupStartRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	st.Requests["start-b1"].Payload = "{"
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("提交内容损坏应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "start-b1") {
		t.Fatalf("错误信息应指出开始请求编号 start-b1，得到 %v", err)
	}
}

// 准备一份只开始、未投料的台账，用于需要单独触发开始请求校验的场景
// （删除批次或把批次改回草稿时，不被投料/关闭请求的校验先拦截）。
func setupStartedOnlyLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s)
	// B1：调整过计划后开始执行，无投料、未关闭。
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2：另一个正常的执行中批次。
	if _, err := s.CreateBatch("b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b2", "B2"); err != nil {
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

// 原请求所指的批次不存在时，保存的开始结果不能单独作为开始成功的依据：
// 即使结果本身内容完整，也按损坏处理，不能凭结果补造批次。
func TestOpenRejectsStartRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupStartedOnlyLedger(t, dir)
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

// 原请求所指的批次被改回草稿（说明第一次开始没有保持住）时，保存的开始
// 结果与批次实际状态不再一致，按损坏处理。
func TestOpenRejectsStartRequestResultWhenBatchBackToDraft(t *testing.T) {
	dir := t.TempDir()
	good := setupStartedOnlyLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	for _, b := range st.Batches {
		if b.BatchNo == "B1" {
			b.Status = StatusDraft
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
		t.Fatalf("批次退回草稿应返回 ErrCorruptData，得到 %v", err)
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

// 批次仍在执行中、但第一次开始之后已经追加了投料：重新打开台账时，第一次
// 开始的保存结果（执行中、空投料、零实投）仍是合法旧结果，不能与批次现状
// （执行中、已有投料）混为一谈；重放返回第一次开始时的内容，查询返回现状。
func TestOpenAcceptsFirstStartResultWhileExecutingWithFeedings(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	first, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatal(err)
	}
	checkFirstStartView(t, first)
	base := replayFeedTime()
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", base, "张三"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("执行中且已有投料的台账应正常打开: %v", err)
	}
	defer s2.Close()

	replay, err := s2.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replay)

	current, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusExecuting || len(current.Feedings) != 1 {
		t.Fatalf("批次现状应为执行中且保留 1 条投料，得到 %+v", current)
	}
	mats := materialsMap(current)
	checkRequirement(t, mats, "M1", "750", "100.5", "-649.5")
	checkRequirement(t, mats, "M4", "6", "0", "-6")
}

// 批次后来的投料与关闭状态属于现状：B1 已投料并关闭，其第一次开始的保存
// 结果仍是执行中、空投料、零实投——这是合法旧结果，不能因与现状不同而被
// 拒绝。重新打开台账后重放仍返回第一次开始时的原始结果，查询仍显示现状。
func TestOpenAcceptsFirstStartResultWhileBatchAdvanced(t *testing.T) {
	dir := t.TempDir()
	good := setupStartRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("第一次开始结果停留在投料与关闭之前不应判为损坏: %v", err)
	}
	defer s.Close()

	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replay)
	// 查询反映的是批次现状：已关闭，投料与关闭记录保持原样。
	checkClosedCurrent(t, mustGetBatch(t, s, "B1"))

	// 关闭后重新打开，行为不变。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("重新打开台账失败: %v", err)
	}
	defer s2.Close()
	replay2, err := s2.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重新打开后重放开始请求应成功: %v", err)
	}
	checkFirstStartView(t, replay2)
	checkClosedCurrent(t, mustGetBatch(t, s2, "B1"))

	// 落盘内容未被“规范化”重写。
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, good) {
		t.Fatalf("只读访问不应改动台账文件")
	}
}

// 正常写法差异不能误判为损坏：开始结果的应投量写成 750.000、实投量写成
// 0.000、差额写成 -750.000，与第一次开始时的记录是同一数量（1 与 1.000、
// 0 与 0.000 表示相同数值，合法负差额不能误报）。台账应正常打开，重放仍
// 返回第一次开始时的完整结果（保留保存的写法）。
func TestOpenAcceptsStartRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupStartRequestLedger(t, dir)

	bad := corruptStartRequestResult(t, good, func() json.RawMessage {
		return mutatedStartView(t, good, func(v *BatchView) {
			v.Materials[0].RequiredGrams = "750.000"
			v.Materials[0].ActualGrams = "0.000"
			v.Materials[0].DifferenceGrams = "-750.000"
			v.Materials[1].RequiredGrams = "6.000"
			v.Materials[1].ActualGrams = "0.000"
			v.Materials[1].DifferenceGrams = "-6.000"
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

	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("重放开始请求应成功: %v", err)
	}
	mats := materialsMap(replay)
	checkRequirement(t, mats, "M1", "750.000", "0.000", "-750.000")
	checkRequirement(t, mats, "M4", "6.000", "0.000", "-6.000")
	// 批次现状不受影响。
	checkClosedCurrent(t, mustGetBatch(t, s, "B1"))
}

// 台账正常打开后，保存内容中的开始请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一个正常批次，也不能忽略损坏的
// 开始请求；被拒绝的重放不得返回损坏的保存结果，被拒绝的写入不留业务变化、
// 不占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，被拒绝过
// 的请求编号可以正常使用，批次后续的投料与关闭记录保持原样。
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
	if _, err := s.AddFeeding("f-b3", "B3", "M1", "1", replayFeedTime(), "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上向正常批次投料应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("f-b3")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原成功请求仍取回第一次开始时的原始结果，B1 后来的投料与关闭
	// 记录保持原样；被拒绝过的请求编号可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.StartBatch("start-b1", "B1")
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkFirstStartView(t, replay)
	checkClosedCurrent(t, mustGetBatch(t, s, "B1"))
	fed, err := s.AddFeeding("f-b3", "B3", "M1", "1", replayFeedTime(), "张三")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if fed.Seq != 1 || fed.MaterialNo != "M1" || fed.Grams != "1" {
		t.Fatalf("B3 投料应正常登记，得到 %+v", fed)
	}
}
