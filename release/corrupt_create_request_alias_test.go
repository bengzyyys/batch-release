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

// 本文件是“两个不同的成功创建批次请求不能指向同一个批次编号”的回归保障：
// 同一批次的首次创建请求若被保存到两个不同的请求编号下，两份记录的内容与
// 保存结果单独核对都能通过（即使提交完全相同、结果都是合法草稿结果、配方
// 份数与数量核对全都一致；或两份提交采用不同配方版本与份数、各自结果仍
// 符合对应提交），随后两个编号都会被当成各自创建成功。打开台账及之后每次
// 查询、写入的重新读取都必须以 ErrCorruptData 拒绝整份台账，错误信息包含
// 批次编号与发生冲突的两个请求编号；不任选一份请求继续，不合并、不改号、
// 不重新创建批次，被拒绝的写入不留业务记录、不占用请求编号。

// setupCreateAliasLedger 在 dir 建立一份正常台账并关闭：
//   - B1（已关闭）：由 create-b1 用 R1/v1、8 份创建，随后草稿调整为 R1/v2、
//     3 份，开始执行、登记一条投料并关闭——批次现状早已离开创建时的计划，
//     但 create-b1 的创建记录仍是合法历史，不能用现在的计划筛掉它；
//   - B2（草稿）：由 create-b2 用 R1/v2、3 份创建；
//   - B3（草稿）：由 create-b3 用 R1/v2、3 份创建——不同批次各自的创建
//     请求，即使用同一配方版本与份数也互不影响。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupCreateAliasLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerCreateReplayRecipes(t, s) // R1/v1：M1=0.125、M2=0.001；R1/v2：M1=250、M4=2
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraftBatch("u-final", "B1", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("f1", "B1", "M1", "100.5", replayFeedTime(), "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-b1", "B1"); err != nil {
		t.Fatal(err)
	}
	// B2、B3：不同批次各自的创建请求，配方版本与份数完全相同。
	if _, err := s.CreateBatch("create-b2", "B2", "R1", "v2", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("create-b3", "B3", "R1", "v2", 3); err != nil {
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

// rewriteCreateAliasLedger 读出正常台账内容，按 mutate 修改请求记录后重新
// 序列化，返回改写后的文件内容。
func rewriteCreateAliasLedger(t *testing.T, good []byte, mutate func(st *persistedState)) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	mutate(&st)
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// duplicateCreateRequestAs 把 create-b1 的请求记录原样复制到另一个请求编号
// 下（操作、提交内容与保存结果完全相同），返回改写后的文件内容。复制出的
// 记录与原记录各自都能通过逐条核对，唯有“两个创建请求指向同一批次”这一
// 跨请求关系不成立。
func duplicateCreateRequestAs(t *testing.T, good []byte, newReqNo string) []byte {
	t.Helper()
	return rewriteCreateAliasLedger(t, good, func(st *persistedState) {
		req := st.Requests["create-b1"]
		if req == nil {
			t.Fatalf("正常台账中应存在 create-b1 请求记录")
		}
		st.Requests[newReqNo] = &requestRecord{Op: req.Op, Payload: req.Payload, Result: req.Result}
	})
}

// forgeSecondCreateRequest 伪造一份指向 B1、但采用 R1/v2 与 3 份的创建请求
// 记录：提交内容以 create-b2 为蓝本改批次编号，保存结果以 create-b2 的合法
// 草稿结果为蓝本改批次编号。伪造记录的提交内容与结果各自核对都合法（对应
// 批次存在、结果符合提交选定的版本与份数），只有跨请求的批次唯一性核对
// 能发现它与 create-b1 指向了同一个批次。
func forgeSecondCreateRequest(t *testing.T, good []byte, newReqNo string) []byte {
	t.Helper()
	return rewriteCreateAliasLedger(t, good, func(st *persistedState) {
		src := st.Requests["create-b2"]
		if src == nil {
			t.Fatalf("正常台账中应存在 create-b2 请求记录")
		}
		var view BatchView
		if err := json.Unmarshal(src.Result, &view); err != nil {
			t.Fatal(err)
		}
		if view.BatchNo != "B2" {
			t.Fatalf("改坏前 create-b2 的结果批次应为 B2，得到 %q", view.BatchNo)
		}
		view.BatchNo = "B1"
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(createBatchPayload{BatchNo: "B1", RecipeNo: "R1", Version: "v2", Portions: 3})
		if err != nil {
			t.Fatal(err)
		}
		st.Requests[newReqNo] = &requestRecord{Op: opCreateBatch, Payload: string(payload), Result: raw}
	})
}

// checkCreateAliasError 校验错误是 ErrCorruptData，且信息中包含批次编号与
// 发生冲突的两个请求编号。
func checkCreateAliasError(t *testing.T, err error, reqNo1, reqNo2, batchNo string) {
	t.Helper()
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("两个创建请求指向同一批次应返回 ErrCorruptData，得到 %v", err)
	}
	msg := err.Error()
	for _, want := range []string{reqNo1, reqNo2, batchNo, "两个成功的创建请求"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，得到 %v", want, err)
		}
	}
}

// 打开台账时发现两个不同的成功创建批次请求指向同一个批次编号（两份记录
// 内容完全相同），必须返回 ErrCorruptData：错误信息包含批次编号与两个
// 请求编号；不返回可用对象，原文件保持不变。
func TestOpenRejectsTwoCreateRequestsForSameBatch(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateAliasLedger(t, dir)
	bad := duplicateCreateRequestAs(t, good, "create-b1-copy")
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	checkCreateAliasError(t, err, "create-b1", "create-b1-copy", "B1")
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 两个创建请求采用不同配方版本与份数、各自保存结果仍符合对应提交时，同样
// 不能解释成两次合法创建：伪造一份指向 B1、采用 R1/v2 与 3 份的创建请求
// （提交与结果各自核对都合法），打开台账必须返回 ErrCorruptData。
func TestOpenRejectsSecondCreateRequestWithDifferentPlan(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateAliasLedger(t, dir)
	bad := forgeSecondCreateRequest(t, good, "create-b1-v2")
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	checkCreateAliasError(t, err, "create-b1", "create-b1-v2", "B1")
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	got, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bad) {
		t.Fatalf("拒绝打开不应改动台账文件")
	}
}

// 台账正常打开后文件才被改坏（create-b1 的记录被复制到另一个请求编号下）：
// 下一次查询或写入重新读取时必须返回 ErrCorruptData——即使访问的是另一个
// 正常批次也不能绕过；被拒绝的重放不得返回任何创建结果，被拒绝的新写入
// 不留业务记录、不占用请求编号。恢复后原请求仍取回首次创建结果，被拒绝过
// 的新请求编号仍可正常使用。
func TestCreateRequestAliasingDetectedOnEveryReload(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateAliasLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 create-b1 的记录复制到另一个请求编号下，其余记录保持完整。
	bad := duplicateCreateRequestAs(t, good, "create-b1-copy")
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询冲突批次与正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B2", "B3"} {
		v, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏未修复时查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号重放：load 先于幂等重放执行，两个指向 B1 的创建请求都不能
	// 把保存的草稿结果当成自己的首次创建返回；正常批次的重放同样不能绕过。
	if _, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("重放 create-b1 应先在重新读取时返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.CreateBatch("create-b2", "B2", "R1", "v2", 3); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放正常批次的创建请求应返回 ErrCorruptData，得到 %v", err)
	}
	// 新写入同样不能绕过。
	if _, err := s.CreateBatch("create-b4", "B4", "R1", "v2", 2); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上写入新批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件，新写入不留下请求记录、不占用请求编号。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("create-b4")) || bytes.Contains(after, []byte("B4")) {
		t.Fatalf("被拒绝的写入不应留下业务记录或占用请求编号")
	}

	// 恢复原文件后：create-b1 仍取回首次创建时的草稿结果（R1/v1、8 份），
	// 被拒绝过的新编号 create-b4 仍可提交并成功创建。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	v1, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("恢复后重放 create-b1 失败: %v", err)
	}
	checkFirstCreateView(t, v1)
	if _, err := s.CreateBatch("create-b4", "B4", "R1", "v2", 2); err != nil {
		t.Fatalf("被拒绝过的请求编号恢复后应可正常使用: %v", err)
	}
	b4, err := s.GetBatch("B4")
	if err != nil {
		t.Fatal(err)
	}
	if b4.Status != StatusDraft || b4.PlannedPortions != 2 {
		t.Fatalf("B4 应为 2 份草稿，得到状态 %q、份数 %d", b4.Status, b4.PlannedPortions)
	}
}

// 不同批次各自保存自己的成功创建请求，即使选用同一配方版本与份数也属正常：
// 台账必须可以打开、重放与查询；草稿调整、开始执行与关闭请求与创建请求
// 引用同一批次（B1 的 u-final、start-b1、close-b1）也属于正常操作，不纳入
// 重复创建判断。
func TestDistinctBatchesWithOwnCreateRequestsAreFine(t *testing.T) {
	dir := t.TempDir()
	setupCreateAliasLedger(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("不同批次各自创建请求的正常台账应能打开: %v", err)
	}
	defer s.Close()

	// create-b1 重放仍返回首次创建时的结果（R1/v1、8 份草稿），不受批次
	// 后来的调整、执行、投料与关闭影响。
	v1, err := s.CreateBatch("create-b1", "B1", "R1", "v1", 8)
	if err != nil {
		t.Fatalf("重放 create-b1 失败: %v", err)
	}
	checkFirstCreateView(t, v1)
	// B2、B3 用同一配方版本与份数各自创建，重放各自取回自己的结果。
	for _, tc := range []struct{ reqNo, batchNo string }{
		{"create-b2", "B2"},
		{"create-b3", "B3"},
	} {
		v, err := s.CreateBatch(tc.reqNo, tc.batchNo, "R1", "v2", 3)
		if err != nil {
			t.Fatalf("重放 %s 失败: %v", tc.reqNo, err)
		}
		if v.BatchNo != tc.batchNo {
			t.Fatalf("重放 %s 应返回批次 %q 的结果，得到 %q", tc.reqNo, tc.batchNo, v.BatchNo)
		}
	}
	// B1 的现状（已关闭、一条投料）不受影响。
	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if b1.Status != StatusClosed || len(b1.Feedings) != 1 {
		t.Fatalf("B1 应为已关闭且有 1 条投料，得到状态 %q、%d 条投料", b1.Status, len(b1.Feedings))
	}
}

// 已有批次没有保存创建请求时继续沿用原来的读取要求：删除 create-b1 的
// 请求记录后，B1 及其投料、后续的调整/开始/关闭请求记录都保留，打开与
// 查询正常，不要求补造创建请求。
func TestBatchWithoutCreateRequestStillReadable(t *testing.T) {
	dir := t.TempDir()
	good := setupCreateAliasLedger(t, dir)

	data := rewriteCreateAliasLedger(t, good, func(st *persistedState) {
		delete(st.Requests, "create-b1")
	})
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("批次缺少创建请求记录不应判为损坏: %v", err)
	}
	defer s.Close()

	b1, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if b1.Status != StatusClosed || len(b1.Feedings) != 1 {
		t.Fatalf("删除创建请求不应改动批次，B1 应为已关闭且有 1 条投料，得到状态 %q、%d 条投料",
			b1.Status, len(b1.Feedings))
	}
	// 其余请求记录的重放不受影响：create-b2 仍取回自己的创建结果。
	v2, err := s.CreateBatch("create-b2", "B2", "R1", "v2", 3)
	if err != nil {
		t.Fatalf("重放 create-b2 失败: %v", err)
	}
	if v2.BatchNo != "B2" {
		t.Fatalf("重放 create-b2 应返回 B2 的结果，得到 %q", v2.BatchNo)
	}
}
