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

// 本文件是“已保存的成功创建批次请求的返回结果必须对应原来那次创建”的回归
// 保障：批次用请求编号创建后，按原编号、原内容再次提交 CreateBatch，只能取
// 回第一次创建成功时的结果（草稿、空投料、原选配方版本、创建时份数、零实投、
// 差额为应投量的负值）。台账里保存的创建结果即使变成缺失、null、空对象，或
// 被改成另一批次（即使名称、物料与数量完全相同）的结果、被改成同编号配方其
// 他版本的结果、被改掉配方绑定/份数/状态，或混入了批次后来调整、开始、投料
// 与关闭后的现状，也不能被当作成功结果返回——打开台账及之后的每次查询/写入
// 重载，都必须核对保存结果确实属于原来的那次创建；不一致按 ErrCorruptData
// 拒绝整份台账，不删除请求、不补造批次，也不改写原文件。

// setupCreateRequestLedger 在 dir 建立一份正常台账：
//   - B1（已关闭）：创建时用 R1/v1、5 份，create-b1 的保存结果必须是首次
//     创建时的内容（R1/v1、5 份、草稿、空投料：M1 应投 0.625、M2 应投
//     2.5）；草稿调整为 R1/v2、3 份后由 start-b1 开始，登记 f1、f2 两条
//     投料并由 close-b1 关闭——后来的调整、投料与关闭都不能混入创建结果；
//   - B2（执行中）：R1/v2、3 份创建后开始，创建结果与 B1 的创建结果在配方
//     与份数上都不同，用于验证另一批次的结果不能顶替；
//   - B3（草稿）：R1/v1、2 份的正常批次，用于验证损坏的创建请求不能被
//     “本次访问的是另一个正常批次”绕过，也用于损坏台账上的新写入被拒绝；
//   - B4（草稿）：R1/v1、5 份，与 B1 创建时的计划完全相同，两份创建结果
//     仅批次编号不同——用于验证另一批次即使名称、物料和数量完全相同也不
//     能顶替。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupCreateRequestLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerReplayRecipes(t, s) // R1/v1：M1=0.125、M2=0.5；R1/v2：M1=250、M4=2
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 5); err != nil {
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
	if _, err := s.CreateBatch("create-b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b2", "B2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("create-b3", "B3", "R1", "v1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("create-b4", "B4", "R1", "v1", 5); err != nil {
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

// createResultRaw 取出正常台账中 reqNo 请求的保存结果原文。
func createResultRaw(t *testing.T, good []byte, reqNo string) json.RawMessage {
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

// mutatedCreateView 读出 create-b1 的保存结果，按 edit 修改后重新序列化。
func mutatedCreateView(t *testing.T, good []byte, edit func(*BatchView)) json.RawMessage {
	t.Helper()
	var v BatchView
	if err := json.Unmarshal(createResultRaw(t, good, "create-b1"), &v); err != nil {
		t.Fatal(err)
	}
	edit(&v)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// corruptCreateRequestResult 把正常台账内容中 create-b1 请求的保存结果替换
// 为 mutate 给出的值（mutate 返回 nil 表示删除该字段），返回改写后的内容。
func corruptCreateRequestResult(t *testing.T, good []byte, mutate func() json.RawMessage) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests["create-b1"]
	if req == nil {
		t.Fatalf("正常台账中应存在 create-b1 请求记录")
	}
	req.Result = mutate()
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// checkFirstCreateInLifecycleView 校验 setupCreateRequestLedger 中“创建 B1
// （R1/v1、5 份）”第一次成功时的完整结果：R1/v1（配方初版）、5 份、草稿、
// 空投料，M1 应投 0.125×5=0.625、M2 应投 0.5×5=2.5，实投为零，差额为应投
// 的负值，不能出现调整后版本独有的 M4。
func checkFirstCreateInLifecycleView(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "R1" || v.RecipeVersion != "v1" || v.RecipeName != "配方初版" {
		t.Fatalf("重放应返回最初创建时的 R1/v1（配方初版），得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 5 {
		t.Fatalf("计划份数应为最初创建的 5，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusDraft {
		t.Fatalf("最初创建的结果应为草稿，得到 %s", v.Status)
	}
	if len(v.Feedings) != 0 {
		t.Fatalf("最初创建时投料列表应为空，得到 %d 条: %+v", len(v.Feedings), v.Feedings)
	}
	if len(v.Materials) != 2 ||
		v.Materials[0].MaterialNo != "M1" || v.Materials[1].MaterialNo != "M2" {
		t.Fatalf("最初版本的物料顺序应为 M1、M2，得到 %+v", v.Materials)
	}
	mats := materialsMap(v)
	checkRequirement(t, mats, "M1", "0.625", "0", "-0.625")
	checkRequirement(t, mats, "M2", "2.5", "0", "-2.5")
	if _, leftover := mats["M4"]; leftover {
		t.Fatalf("创建结果不能带入调整后版本独有的物料 M4: %+v", mats["M4"])
	}
}

// 已保存的成功创建请求的结果缺失、为 null、为空对象、无法读成批次结果，
// 或与原提交不一致（批次编号、配方绑定、份数、状态、投料列表、逐物料应投量/
// 实投量/差额有任何差异，含被另一批次——哪怕名称、物料与数量完全相同——
// 顶替、被同编号配方其他版本顶替、混入后来调整/开始/投料/关闭后的现状）时，
// Open 必须返回 ErrCorruptData：错误信息指出问题请求编号及关联批次，不返回
// 可用的台账对象，也不改写原文件。
func TestOpenRejectsCorruptCreateRequestResult(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(good []byte) json.RawMessage
	}{
		{"结果缺失", func(good []byte) json.RawMessage { return nil }},
		{"结果为 null", func(good []byte) json.RawMessage { return json.RawMessage("null") }},
		{"结果为空对象", func(good []byte) json.RawMessage { return json.RawMessage(`{}`) }},
		{"结果不是批次结果对象", func(good []byte) json.RawMessage { return json.RawMessage(`"not-an-object"`) }},
		{"结果被替换成另一批次的创建结果", func(good []byte) json.RawMessage {
			// B2 用 R1/v2、3 份创建，与 B1 的创建结果不同，那是另一批次。
			return createResultRaw(t, good, "create-b2")
		}},
		{"结果被替换成内容完全相同的另一批次创建结果", func(good []byte) json.RawMessage {
			// B4 与 B1 创建时同为 R1/v1、5 份，两份结果仅批次编号不同。
			return createResultRaw(t, good, "create-b4")
		}},
		{"结果被替换成该批次后来开始执行的结果", func(good []byte) json.RawMessage {
			return createResultRaw(t, good, "start-b1")
		}},
		{"结果批次编号被改成另一批次", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.BatchNo = "B2" })
		}},
		{"结果配方编号与原提交不符", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.RecipeNo = "RX" })
		}},
		{"结果沿用草稿调整后的版本", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.RecipeVersion = "v2" })
		}},
		{"结果配方名称被改", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.RecipeName = "被改的名称" })
		}},
		{"结果沿用草稿调整后的份数", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.PlannedPortions = 3 })
		}},
		{"结果状态被改成执行中", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Status = StatusExecuting })
		}},
		{"结果状态被改成关闭后的现状", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Status = StatusClosed })
		}},
		{"结果混入了后来追加的投料", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				base := replayFeedTime()
				v.Feedings = []FeedingView{
					{Seq: 1, MaterialNo: "M1", Grams: "100.5", Time: base, Registrar: "张三"},
					{Seq: 2, MaterialNo: "M4", Grams: "2", Time: base, Registrar: "李四"},
				}
			})
		}},
		{"物料核对项少一项", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				v.Materials = v.Materials[:1]
			})
		}},
		{"物料核对项混入调整后版本的物料", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				v.Materials = append(v.Materials, MaterialRequirement{
					MaterialNo: "M4", RequiredGrams: "6", ActualGrams: "0", DifferenceGrams: "-6",
				})
			})
		}},
		{"物料核对顺序被调换", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) {
				v.Materials[0], v.Materials[1] = v.Materials[1], v.Materials[0]
			})
		}},
		{"应投量被改成调整后计划的值", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Materials[0].RequiredGrams = "750" })
		}},
		{"实投量被改成非零", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Materials[0].ActualGrams = "100.5" })
		}},
		{"差额被改坏", func(good []byte) json.RawMessage {
			return mutatedCreateView(t, good, func(v *BatchView) { v.Materials[0].DifferenceGrams = "0" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupCreateRequestLedger(t, dir)
			bad := corruptCreateRequestResult(t, good, func() json.RawMessage { return tc.mutate(good) })
			if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
				t.Fatal(err)
			}

			s, err := Open(dir)
			if !errors.Is(err, ErrCorruptData) {
				t.Fatalf("保存的创建结果损坏应返回 ErrCorruptData，得到 %v", err)
			}
			if s != nil {
				s.Close()
				t.Fatalf("损坏台账不应返回可用的 Store 对象")
			}
			msg := err.Error()
			for _, want := range []string{"create-b1", "B1"} {
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

// 原请求对应的批次不存在时，保存的创建结果不能单独作为创建成功的依据：
// 即使结果本身内容完整，也按损坏处理。
func TestOpenRejectsCreateRequestResultWhenBatchMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 B1（create-b1 及 start-b1、close-b1、f1、f2 对应的批次）；该批次
	// 后续操作的请求一并移除，让 create-b1 成为唯一的损坏点。保留 B2、B3、B4。
	kept := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B1" {
			kept = append(kept, b)
		}
	}
	st.Batches = kept
	for _, reqNo := range []string{"start-b1", "close-b1", "f1", "f2"} {
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
		t.Fatalf("原批次不存在但保留创建结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"create-b1", "B1"} {
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

// 原提交所选的配方版本不存在时，保存的创建结果不能单独作为创建成功的依据，
// 同编号配方的其他版本（R1/v2）不能顶替：即使结果本身内容完整，也按损坏
// 处理，错误信息指出请求、批次及原选配方编号与版本号。
func TestOpenRejectsCreateRequestResultWhenRecipeVersionMissing(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	// 删除 R1/v1：B1 现已改绑 R1/v2 不受影响；引用 R1/v1 的 B3、B4 两批次
	// 及其创建请求、R1/v1 的配方登记请求一并移除，让 create-b1 成为唯一的
	// 损坏点（同编号的 R1/v2 仍完整保留）。
	keptRecipes := st.Recipes[:0]
	for _, r := range st.Recipes {
		if !(r.RecipeNo == "R1" && r.Version == "v1") {
			keptRecipes = append(keptRecipes, r)
		}
	}
	st.Recipes = keptRecipes
	keptBatches := st.Batches[:0]
	for _, b := range st.Batches {
		if b.BatchNo != "B3" && b.BatchNo != "B4" {
			keptBatches = append(keptBatches, b)
		}
	}
	st.Batches = keptBatches
	for _, reqNo := range []string{"recipe-v1", "create-b3", "create-b4"} {
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
		t.Fatalf("原选版本不存在但保留创建结果应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"create-b1", "B1", "R1", "v1"} {
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

// 原提交内容无法解析，或解析后不符合创建要求（关键字段为空、份数非正）时，
// 无法核对“原来的那次创建”，按损坏处理；无法确定批次时错误信息至少指出
// 请求编号。
func TestOpenRejectsCorruptCreateRequestPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		batchOK bool // 提交内容里是否仍能确定批次编号
	}{
		{"提交内容无法解析", `{坏`, false},
		{"份数为零", `{"BatchNo":"B1","RecipeNo":"R1","Version":"v1","Portions":0}`, true},
		{"份数为负数", `{"BatchNo":"B1","RecipeNo":"R1","Version":"v1","Portions":-3}`, true},
		{"批次编号为空", `{"BatchNo":"","RecipeNo":"R1","Version":"v1","Portions":5}`, false},
		{"配方编号为空", `{"BatchNo":"B1","RecipeNo":"","Version":"v1","Portions":5}`, true},
		{"版本号为空", `{"BatchNo":"B1","RecipeNo":"R1","Version":"","Portions":5}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			good := setupCreateRequestLedger(t, dir)
			var st persistedState
			if err := json.Unmarshal(good, &st); err != nil {
				t.Fatal(err)
			}
			st.Requests["create-b1"].Payload = tc.payload
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
			if !strings.Contains(err.Error(), "create-b1") {
				t.Fatalf("错误信息应包含请求编号 create-b1，得到 %v", err)
			}
			if tc.batchOK && !strings.Contains(err.Error(), "B1") {
				t.Fatalf("能确定批次时错误信息应包含 B1，得到 %v", err)
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

// 正常写法差异不能误判为损坏：保存结果的应投量写成 0.625 等价的 0.625 形式、
// 2.500、实投量写成 0.000、差额写成 -2.500，与首次创建时的数量是同一数值；
// 合法的零实投与负差额不是损坏。台账应正常打开，重放仍返回保存的写法。
func TestOpenAcceptsCreateRequestResultFormatVariants(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)

	bad := corruptCreateRequestResult(t, good, func() json.RawMessage {
		return mutatedCreateView(t, good, func(v *BatchView) {
			v.Materials[0].RequiredGrams = "0.625" // 与保存的 0.625 同值
			v.Materials[0].ActualGrams = "0.000"   // 与 0 是同一数量
			v.Materials[0].DifferenceGrams = "-0.625"
			v.Materials[1].RequiredGrams = "2.500" // 与 2.5 是同一数量
			v.Materials[1].ActualGrams = "0.000"
			v.Materials[1].DifferenceGrams = "-2.500"
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

	// 重放取回保存的结果原文：核对项保持改写后的写法，但与首次创建时的
	// 数量是同一数值。
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 5)
	if err != nil {
		t.Fatalf("重放创建请求应成功: %v", err)
	}
	if replay.Materials[0].RequiredGrams != "0.625" ||
		replay.Materials[0].ActualGrams != "0.000" ||
		replay.Materials[0].DifferenceGrams != "-0.625" ||
		replay.Materials[1].RequiredGrams != "2.500" ||
		replay.Materials[1].DifferenceGrams != "-2.500" {
		t.Fatalf("重放应返回保存的核对项写法，得到 %+v", replay.Materials)
	}
	// 批次后来的调整、投料与关闭属于现状：查询显示已关闭的 R1/v2、3 份。
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))
}

// 批次后来改选配方、调整份数，甚至已开始执行、追加投料并关闭，都属于批次
// 现状，不能混入第一次创建的结果，也不能成为拒绝合法旧结果的理由：台账中
// B1 已关闭，其保存的创建结果仍是首次创建时的内容（草稿、空投料、R1/v1、
// 5 份），打开与重放都必须正常；重复提交原创建请求返回首次结果，查询继续
// 显示当前批次，两者不能互相覆盖。
func TestCreateRequestResultStaysFirstCreateAfterLifecycle(t *testing.T) {
	dir := t.TempDir()
	setupCreateRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("已走完生命周期的批次不应让其首次创建结果被判损坏: %v", err)
	}
	defer s.Close()

	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 5)
	if err != nil {
		t.Fatalf("重放创建请求应成功: %v", err)
	}
	checkFirstCreateInLifecycleView(t, replay)
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))

	// 再走一轮：历史结果与当前查询仍互不覆盖。
	replay2, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 5)
	if err != nil {
		t.Fatalf("再次重放创建请求应成功: %v", err)
	}
	checkFirstCreateInLifecycleView(t, replay2)
	checkClosedCurrentAfterSetup(t, mustGetBatch(t, s, "B1"))
}

// 台账正常打开后，保存内容中的创建请求结果被改坏：下一次查询或写入必须
// 返回 ErrCorruptData——即使本次访问的是另一个正常批次，也不能忽略损坏的
// 创建请求；被拒绝的重放不得返回损坏的保存结果，被拒绝的写入不留业务变化、
// 不占用请求编号，原台账内容不变。恢复后原成功请求仍可幂等重放，被拒绝过
// 的请求编号可以正常使用。
func TestCreateRequestResultCorruptionAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateRequestLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 create-b1 的保存结果改坏为空对象（其余记录保持完整）。
	bad := corruptCreateRequestResult(t, good, func() json.RawMessage {
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
	// 用原编号、原内容重放损坏的创建请求：不得把损坏的保存结果当成成功
	// 结果返回，必须报损坏。
	if v, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 5); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放损坏的创建请求应返回 ErrCorruptData，得到 %v", err)
	} else if v != nil {
		t.Fatalf("被拒绝的重放不应返回部分结果: %+v", v)
	}
	// 对正常批次的新写入同样不能绕过（B3 是正常草稿，调整份数本应成功）。
	if _, err := s.UpdateDraftBatch("u-b3", "B3", "", "", 4); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上调整正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("u-b3")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：原成功请求仍取回第一次创建时的结果，被拒绝过的请求编号
	// 可以正常使用。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 5)
	if err != nil {
		t.Fatalf("恢复后重放原成功请求失败: %v", err)
	}
	checkFirstCreateInLifecycleView(t, replay)
	adjusted, err := s.UpdateDraftBatch("u-b3", "B3", "", "", 4)
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if adjusted.BatchNo != "B3" || adjusted.PlannedPortions != 4 || adjusted.Status != StatusDraft {
		t.Fatalf("B3 应被正常调整为 4 份草稿，得到 %+v", adjusted)
	}
}
