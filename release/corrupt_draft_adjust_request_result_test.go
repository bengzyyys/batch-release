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

// 本文件是“已保存的成功草稿调整请求的返回结果必须对应原提交内容调整成功时
// 的那份批次结果”的回归保障：草稿调整成功后，台账保存当次返回的批次结果供
// 同一请求再次提交时取回。这份结果即使被改成空对象、null、另一批次的结果，
// 或被改掉批次编号、配方绑定、名称、份数、状态，混入批次后来的开始、投料与
// 关闭现状，也不能被当作成功结果返回——打开台账及之后的每次查询/写入重载，
// 都必须核对保存结果：原调整请求对应的批次必须存在，结果批次编号与之一致，
// 结果为草稿、没有投料，采用的配方版本仍登记在案、名称与该版本一致、计划
// 份数为正整数，原请求明确指定的新配方/新份数必须在结果中反映，逐物料按
// 结果采用版本的顺序列出应投量（每份克数 × 份数）、实投为零、差额为应投的
// 负值；不一致按 ErrCorruptData 拒绝整份台账，不删除请求、不补造批次，也
// 不用当前查询结果覆盖损坏结果。

// setupAdjustRequestLedger 在 dir 建立一份正常台账：
//   - B1（已关闭）：create-b1 用 R1/v2、5 份创建，u-first 调整为 R1/v1、
//     8 份（其保存结果必须始终是这次调整的草稿结果：R1/v1、8 份、空投料、
//     M1=1、M2=4），u-final 又调整为 R1/v2、3 份，随后开始执行、登记两条
//     投料并关闭；
//   - B2（草稿）：create-b2 用 R1/v2、5 份创建，u-b2 调整为 R1/v1、8 份
//     ——u-b2 的结果与 u-first 的配方、份数、数量完全相同，仅批次编号不同，
//     用于验证另一批次的结果不能顶替；
//   - B3（草稿）：R1/v2、2 份的正常批次，不参与任何改坏，用于验证损坏的
//     调整请求不能被“本次访问的是另一条正常记录”绕过。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupAdjustRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s) // R1/v1：M1=0.125、M2=0.5；R1/v2：M1=250、M4=2
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v2", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); err != nil {
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
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2：调整后的配方与份数和 u-first 的结果相同，仅批次编号与请求编号不同。
	if _, err := s.CreateBatch("create-b2", "B2", "R1", "v2", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-b2", "B2", "R1", "v1", 8); err != nil {
		t.Fatal(err)
	}
	// B3：正常批次，不参与任何改坏。
	if _, err := s.CreateBatch("create-b3", "B3", "R1", "v2", 2); err != nil {
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

// mutatedBatchResult 读出 reqNo 请求保存的批次结果，按 edit 修改后重新序列化。
func mutatedBatchResult(t *testing.T, good []byte, reqNo string, edit func(*BatchView)) json.RawMessage {
	t.Helper()
	var v BatchView
	if err := json.Unmarshal(createResultRaw(t, good, reqNo), &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// corruptRequestResult 把正常台账中 targetReq 请求的保存结果替换为 mutate
// 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的文件内容。
func corruptRequestResult(t *testing.T, good []byte, targetReq string, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[targetReq]
	if req == nil {
		t.Fatalf("正常台账中应存在 %s 请求记录", targetReq)
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// corruptAdjustRequestResult 改坏 u-first 请求的保存结果。
func corruptAdjustRequestResult(t *testing.T, good []byte, mutate func() json.RawMessage) []byte {
	t.Helper()
	return corruptRequestResult(t, good, "u-first", mutate)
}

// 已保存的成功草稿调整请求的结果缺失、为 null、为空对象、无法读成批次结果，
// 或与原提交内容调整成功时的结果不一致（批次编号、配方绑定、名称、计划份数、
// 状态、投料列表、逐物料应投量/实投量/差额有任何差异，含被另一批次的调整
// 结果顶替、采用未登记版本或其他版本、混入后来的开始/投料/关闭现状）时，
// Open 必须返回 ErrCorruptData：错误信息指出问题请求编号及能确定的批次编号，
// 不返回可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptAdjustRequestResult(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(good []byte) json.RawMessage
	}{
		{"结果缺失", func(good []byte) json.RawMessage { return nil }},
		{"结果为 null", func(good []byte) json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func(good []byte) json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是批次结果对象", func(good []byte) json.RawMessage {
			return json.RawMessage(`"not-an-object"`)
		}},
		{"结果被替换成另一批次的调整结果", func(good []byte) json.RawMessage {
			// B2 的调整结果配方、份数、数量与 u-first 相同，但那是另一批次。
			return createResultRaw(t, good, "u-b2")
		}},
		{"结果批次编号被改成另一批次", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) { v.BatchNo = "B2" })
		}},
		{"结果采用的配方版本未登记", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.RecipeNo = "R9"
				v.RecipeVersion = "v1"
			})
		}},
		{"结果改用同编号的其他已登记版本", func(good []byte) json.RawMessage {
			// 名称一并改成 v2 的名称，使其精确落到“与原提交指定版本不符”。
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.RecipeVersion = "v2"
				v.RecipeName = "配方改版"
			})
		}},
		{"结果配方名称被改", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) { v.RecipeName = "被改的名称" })
		}},
		{"结果份数被改成另一合法份数", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) { v.PlannedPortions = 3 })
		}},
		{"结果份数被改成零", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) { v.PlannedPortions = 0 })
		}},
		{"结果份数被改成负数", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) { v.PlannedPortions = -2 })
		}},
		{"结果状态被改成关闭后的现状", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) { v.Status = StatusClosed })
		}},
		{"结果状态被改成执行中", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) { v.Status = StatusExecuting })
		}},
		{"结果混入了后来追加的投料", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				base := replayFeedTime()
				v.Feedings = []FeedingView{
					{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: base, Registrar: "张三"},
					{Seq: 2, MaterialNo: "M4", Grams: "2", Time: base, Registrar: "李四"},
				}
			})
		}},
		{"物料核对项少一项", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.Materials = v.Materials[:1]
			})
		}},
		{"物料核对项混入其他版本的物料", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.Materials = append(v.Materials, MaterialRequirement{
					MaterialNo: "M4", RequiredGrams: "6", ActualGrams: "0", DifferenceGrams: "-6",
				})
			})
		}},
		{"物料核对顺序被调换", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.Materials[0], v.Materials[1] = v.Materials[1], v.Materials[0]
			})
		}},
		{"应投量被改坏", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].RequiredGrams = "2"
			})
		}},
		{"实投量被改成非零", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].ActualGrams = "0.5"
			})
		}},
		{"差额被改坏", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].DifferenceGrams = "0"
			})
		}},
		{"核对数量写成无法解析的字符串", func(good []byte) json.RawMessage {
			return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
				v.Materials[0].RequiredGrams = "abc"
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupAdjustRequestLedger(t, dir)
			bad := corruptAdjustRequestResult(t, good, func() json.RawMessage { return tc.mutate(good) })
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存的调整结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"u-first", "B1"} {
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

// 原提交内容本身被改坏（无法解析，或改选配方只给编号不给版本号、份数为负等
// 不可能成功的内容）时，不可能对应一次成功的草稿调整：Open 必须返回
// ErrCorruptData。无法解析提交内容时无法确定批次，错误信息至少指出请求编号。
func TestOpenRejectsCorruptAdjustRequestPayload(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantBatchNo bool
	}{
		{"提交内容无法解析", `not-json`, false},
		{"改选配方只给编号不给版本号", `{"BatchNo":"B1","RecipeNo":"R1","Version":"","Portions":8}`, true},
		{"改选配方只给版本号不给编号", `{"BatchNo":"B1","RecipeNo":"","Version":"v1","Portions":8}`, true},
		{"提交内容份数为负", `{"BatchNo":"B1","RecipeNo":"R1","Version":"v1","Portions":-2}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupAdjustRequestLedger(t, dir)
			var st persistedState
			if err := json.Unmarshal(good, &st); err != nil {
				t.Fatal(err)
			}
			st.Requests["u-first"].Payload = tc.payload
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
			msg := err.Error()
			if !strings.Contains(msg, "u-first") {
				t.Fatalf("错误信息应指出问题请求编号 u-first，得到 %v", err)
			}
			if tc.wantBatchNo && !strings.Contains(msg, "B1") {
				t.Fatalf("错误信息应包含能确定的批次编号 B1，得到 %v", err)
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

// 原调整请求对应的批次不存在时，保存的调整结果不能单独作为调整成功的依据：
// 即使结果本身内容完整，也按损坏处理。
func TestOpenRejectsAdjustRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupAdjustRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 B1，并移除依附于它的其他请求记录（创建、后续调整、开始、投料、
	// 关闭），只保留 u-first，让“批次缺失”成为唯一的损坏点。
	kept := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B1" {
			kept = append(kept, b)
		}
	}
	st.Batches = kept
	for _, reqNo := range []string{"create-b1", "u-final", "start-b1", "f1", "f2", "close-b1"} {
		delete(st.Requests, reqNo)
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
		t.Fatalf("原批次不存在但保留调整结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"u-first", "B1"} {
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

// 结果采用的配方版本被从台账中删除时，保存结果里的名称与物料用量失去依据：
// 批次后来已改回另一版本、批次绑定仍然完整，也不能让这次采用了缺失版本的
// 调整结果通过——同编号配方的其他版本不能顶替。
func TestOpenRejectsAdjustRequestResultWhenAdoptedVersionMissing(t *testing.T) {
	dir := t.TempDir()
	// 专用台账：B1 创建时用 R1/v2，u-first 改为 R1/v1、8 份，u-final 又改回
	// R1/v2、3 份（批次当前绑定 v2），没有开始与投料。
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v2", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 R1/v1 及其配方登记请求：批次 B1 当前绑定 v2 仍完整，只有 u-first
	// 的结果采用了被删除的 v1。
	kept := st.Recipes[:0]
	for _, r := range st.Recipes {
		if !(r.RecipeNo == "R1" && r.Version == "v1") {
			kept = append(kept, r)
		}
	}
	st.Recipes = kept
	delete(st.Requests, "recipe-v1")
	bad, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("结果采用的版本缺失应返回 ErrCorruptData，得到 %v", err)
	}
	if s2 != nil {
		s2.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"u-first", "B1", "R1", "v1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
}

// 正常写法差异不能误判为损坏：保存结果的应投量写成 1.000、实投量写成
// 0.000、差额写成 -1.000，与调整成功时的数量是同一数值；合法的零实投与
// 负差额不是损坏。台账应正常打开，重放仍返回保存的写法。
func TestOpenAcceptsAdjustRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupAdjustRequestLedger(t, dir)

	bad := corruptAdjustRequestResult(t, good, func() json.RawMessage {
		return mutatedBatchResult(t, good, "u-first", func(v *BatchView) {
			v.Materials[0].RequiredGrams = "1.000" // 与 1 是同一数量
			v.Materials[0].ActualGrams = "0.000"   // 与 0 是同一数量
			v.Materials[0].DifferenceGrams = "-1.000"
			v.Materials[1].RequiredGrams = "4.000" // 与 4 是同一数量
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

	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放调整请求应成功: %v", err)
	}
	if replay.Materials[0].RequiredGrams != "1.000" ||
		replay.Materials[0].ActualGrams != "0.000" ||
		replay.Materials[0].DifferenceGrams != "-1.000" {
		t.Fatalf("重放应返回保存的核对项写法，得到 %+v", replay.Materials[0])
	}
}

// 批次后来再次调整、开始执行、登记投料并关闭都属于批次现状，不能混入这次
// 调整的结果，也不能成为拒绝一份合法旧结果的理由：台账中 B1 已经历 u-first
// （R1/v1、8 份）、u-final（R1/v2、3 份）、开始、两条投料并关闭，其保存的
// u-first 结果仍是当时的草稿内容（R1/v1、8 份、空投料、零实投），打开与
// 重放都必须正常；重复提交返回当次成功结果，查询继续显示当前批次，不回退
// 状态或改写投料。
func TestAdjustRequestResultStaysValidAfterLaterAdjustAndClose(t *testing.T) {
	dir := t.TempDir()
	good := setupAdjustRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("已再次调整并关闭的批次不应让早先合法的草稿调整结果被判损坏: %v", err)
	}
	defer s.Close()
	if string(good) == "" {
		t.Fatal("正常台账内容不应为空")
	}

	// 重复提交第一次成功的调整：仍返回当次成功结果（R1/v1、8 份、草稿）。
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放调整请求应成功: %v", err)
	}
	checkFirstAdjustView(t, replay)

	// 查询继续显示当前批次（已关闭、R1/v2、3 份、两条投料），不被历史结果回退。
	got, err := s.GetBatch("B1")
	if err != nil {
		t.Fatalf("查询当前批次失败: %v", err)
	}
	if got.BatchNo != "B1" || got.RecipeNo != "R1" || got.RecipeVersion != "v2" ||
		got.PlannedPortions != 3 || got.Status != StatusClosed {
		t.Fatalf("查询应显示当前已关闭批次（R1/v2、3 份），得到 %+v", got)
	}
	if len(got.Feedings) != 2 {
		t.Fatalf("当前批次应保留 2 条投料，得到 %d 条", len(got.Feedings))
	}
	mats := materialsMap(got)
	checkRequirement(t, mats, "M1", "750", "100.5", "-649.5") // 250×3
	checkRequirement(t, mats, "M4", "6", "2", "-4")           // 2×3
}

// 沿用字段（配方编号与版本同时留空、份数传 0）的调整语义保持不变：沿用值
// 只须符合完整性与数量规则，不要求与批次当前计划相同。批次后来再次调整、
// 开始、投料并关闭后，早先沿用字段的合法草稿结果仍不能被误判为损坏；重放
// 返回当时确定的结果，查询显示当前批次。
func TestAdjustRequestResultWithInheritedParamsStaysValid(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registerReplayRecipes(t, s)
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	// 配方留空：沿用 R1/v1，份数改为 8。
	if _, err := s.UpdateDraftBatch("u-keep-recipe", "B1", "", "", 8); err != nil {
		t.Fatal(err)
	}
	// 份数传 0：沿用 8 份，配方改为 R1/v2。
	if _, err := s.UpdateDraftBatch("u-keep-portions", "B1", "R1", "v2", 0); err != nil {
		t.Fatal(err)
	}
	// 批次后来再改为 R1/v2、4 份，开始、投料并关闭。
	if _, err := s.UpdateDraftBatch("u-later", "B1", "", "", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f1", "B1", "M1", "100", replayFeedTime(), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("沿用字段的合法草稿结果在批次后续演变后不应被判损坏: %v", err)
	}
	defer s2.Close()

	// 重放“沿用配方”的结果：当时确定的 R1/v1、8 份，不按当前 v2 套用留空。
	replayRecipe, err := s2.UpdateDraftBatch("u-keep-recipe", "B1", "", "", 8)
	if err != nil {
		t.Fatalf("重放沿用配方的请求应成功: %v", err)
	}
	checkFirstAdjustView(t, replayRecipe)

	// 重放“沿用份数”的结果：当时沿用的 8 份，不按当前 4 份套用传 0。
	replayPortions, err := s2.UpdateDraftBatch("u-keep-portions", "B1", "R1", "v2", 0)
	if err != nil {
		t.Fatalf("重放沿用份数的请求应成功: %v", err)
	}
	if replayPortions.RecipeNo != "R1" || replayPortions.RecipeVersion != "v2" ||
		replayPortions.PlannedPortions != 8 || replayPortions.Status != StatusDraft {
		t.Fatalf("重放结果应是当时的 R1/v2、8 份、草稿，得到 %+v", replayPortions)
	}
	rpMats := materialsMap(replayPortions)
	checkRequirement(t, rpMats, "M1", "2000", "0", "-2000") // 250 × 8
	checkRequirement(t, rpMats, "M4", "16", "0", "-16")     // 2 × 8

	// 查询仍显示当前已关闭批次（R1/v2、4 份、投料保留）。
	got, err := s2.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RecipeVersion != "v2" || got.PlannedPortions != 4 || got.Status != StatusClosed {
		t.Fatalf("查询应显示当前已关闭批次，得到 %+v", got)
	}
	if len(got.Feedings) != 1 || got.Feedings[0].MaterialNo != "M1" ||
		got.Feedings[0].Grams != "100" {
		t.Fatalf("当前批次投料应保留，得到 %+v", got.Feedings)
	}
}

// 台账正常打开后，保存内容中的调整请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一条正常批次，也不能忽略损坏的
// 调整请求；被拒绝的重放不得返回损坏结果，被拒绝的写入不留业务变化、不
// 占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，被拒绝过的
// 请求编号可以正常使用。
func TestAdjustRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupAdjustRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 u-first 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptAdjustRequestResult(t, good, func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与正常批次都必须失败，不能沿用此前读到的内容，也不能跳过
	// 损坏结果继续使用另一条正常记录。
	for _, batchNo := range []string{"B1", "B3"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号、原内容重放损坏的调整请求：不得把损坏的保存结果当成成功结果
	// 返回，必须报损坏。
	if _, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的调整请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 对正常批次的新写入同样不能绕过损坏结果。
	if _, err := s.StartBatch("start-b3", "B3"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上开始正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号、不留下业务变更。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("start-b3")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原成功请求仍取回当次调整结果，被拒绝过的请求编号可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.UpdateDraftBatch("u-first", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkFirstAdjustView(t, replay)
	started, err := s.StartBatch("start-b3", "B3")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if started.Status != StatusExecuting || started.BatchNo != "B3" {
		t.Fatalf("B3 应被正常开始执行，得到 %+v", started)
	}
}

// 任何一条成功调整请求损坏都应拒绝整份台账：改坏 u-b2 的结果后，即使调用方
// 只查询/操作正常的 B1、B3，Open 也必须返回 ErrCorruptData。
func TestAnyCorruptAdjustRequestRejectsWholeLedger(t *testing.T) {
	dir := t.TempDir()
	good := setupAdjustRequestLedger(t, dir)
	bad := corruptRequestResult(t, good, "u-b2", func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("任一调整请求损坏都应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	if !strings.Contains(err.Error(), "u-b2") {
		t.Fatalf("错误信息应指出问题请求编号 u-b2，得到 %v", err)
	}
}

// 白盒核对：正常台账中 u-first 的保存结果确实是这次调整成功时的内容，且本
// 文件的改坏辅助函数确实改动了文件内容（防止测试因辅助函数失效而假性通过）。
func TestAdjustRequestCorruptionHelpersWork(t *testing.T) {
	dir := t.TempDir()
	good := setupAdjustRequestLedger(t, dir)

	var saved BatchView
	if err := json.Unmarshal(createResultRaw(t, good, "u-first"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.BatchNo != "B1" || saved.RecipeNo != "R1" || saved.RecipeVersion != "v1" ||
		saved.RecipeName != "配方初版" || saved.PlannedPortions != 8 ||
		saved.Status != StatusDraft || len(saved.Feedings) != 0 || len(saved.Materials) != 2 {
		t.Fatalf("正常台账中保存的调整结果应为这次调整成功时的内容，得到 %+v", saved)
	}
	bad := corruptAdjustRequestResult(t, good, func() json.RawMessage {
		return json.RawMessage(`{}`)
	})
	if bytes.Equal(good, bad) {
		t.Fatal("改坏辅助函数未改动台账内容")
	}
	// 正常台账本身必须能打开：所有“正常后续操作不误判”的前提。
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("正常台账应能打开: %v", err)
	}
	s.Close()
}
