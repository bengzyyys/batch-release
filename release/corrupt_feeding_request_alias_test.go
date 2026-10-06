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

// 本文件是“两个不同的成功投料请求不能指向同一条投料登记”的回归保障：
// 同一批次用两个不同请求编号登记内容完全相同的投料，本应得到两个连续序号；
// 若把第二次请求保存结果的序号改成第一次的序号、其余内容（物料、克数、
// 时间、登记人）保持一致，则每个请求的内容与结果单独看都能通过现有逐项
// 核对，批次里的两条投料也都还在、累计数量正确——但两个请求随后都会返回
// 第一条投料，这不能被当作两次各自成功的登记。打开台账及之后每次查询、
// 写入的重新读取都必须以 ErrCorruptData 拒绝整份台账，错误信息包含两个
// 请求编号、批次编号与重复的登记序号；不删除请求、不合并投料、不替结果
// 重新分配序号，被拒绝的写入不留业务记录、不占用请求编号。

// feedingAliasBaseTime 是本文件测试使用的固定投料时间。
func feedingAliasBaseTime() time.Time {
	return time.Date(2026, 10, 5, 9, 30, 0, 0, time.UTC)
}

// setupFeedingAliasLedger 在 dir 建立一份正常台账并关闭：
//   - B1（执行中）：feed-1 与 feed-2 用完全相同的内容（M1、1.000 克、
//     同一时刻、同一登记人）各登记一次，分别得到序号 1、2；
//   - B2（执行中）：feed-b2 以完全相同的投料内容登记，得到 B2 自己的
//     序号 1——不同批次各自从 1 开始，即使内容相同也不属于重复指向。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupFeedingAliasLedger(t *testing.T, dir string) []byte {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	registerStandardRecipe(t, s, "recipe-1") // M1=100、M2=0.5、M3=0.010
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("b2", "B2", "R1", "v1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s2", "B2"); err != nil {
		t.Fatal(err)
	}
	tm := feedingAliasBaseTime()
	// 两条内容完全相同的投料（连时间与登记人都相同），仅请求编号不同：
	// 正常规则下它们是两次各自成功的登记，序号分别为 1 和 2。
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-2", "B1", "M1", "1.000", tm, "张三"); err != nil {
		t.Fatal(err)
	}
	// 另一批次以相同内容登记，序号同样为 1，但指向的是 B2 自己的投料。
	if _, err := s.AddFeeding("feed-b2", "B2", "M1", "1.000", tm, "张三"); err != nil {
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

// retargetFeedingRequestResult 把正常台账中指定投料请求的保存结果按 mutate
// 改写，返回改写后的文件内容。本文件用它把 feed-2 结果的序号从 2 改成 1。
func retargetFeedingRequestResult(t *testing.T, good []byte, reqNo string, mutate func(*FeedingView)) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[reqNo]
	if req == nil {
		t.Fatalf("正常台账中应存在 %q 请求记录", reqNo)
	}
	var view FeedingView
	if err := json.Unmarshal(req.Result, &view); err != nil {
		t.Fatal(err)
	}
	mutate(&view)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	req.Result = raw
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// aliasFeedingResultToFirst 把 feed-2 保存结果的登记序号改成 1，其余内容
// （物料、克数、时间、登记人）保持不变。由于 feed-2 与 feed-1 原提交内容
// 完全相同，改完后 feed-2 的结果既符合自己的原提交内容，又与 B1 序号 1
// 的实际投料逐项一致——单请求核对全部通过，唯有“两个请求指向同一条投料”
// 这一跨请求关系不成立。B1 的两条投料记录与累计数量都保持原样。
func aliasFeedingResultToFirst(t *testing.T, good []byte) []byte {
	t.Helper()
	return retargetFeedingRequestResult(t, good, "feed-2", func(v *FeedingView) {
		if v.Seq != 2 {
			t.Fatalf("改坏前 feed-2 的结果序号应为 2，得到 %d", v.Seq)
		}
		v.Seq = 1
	})
}

// 打开台账时发现两个不同的成功投料请求指向同一批次的同一条投料登记，
// 必须返回 ErrCorruptData：错误信息说明两个请求指向了同一条投料，并包含
// 两个请求编号、批次编号与重复的登记序号；不返回可用对象，原文件保持不变。
func TestOpenRejectsTwoFeedingRequestsAliasingSameFeeding(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	bad := aliasFeedingResultToFirst(t, good)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("两个投料请求指向同一条投料应返回 ErrCorruptData，得到 %v", err)
	}
	if s != nil {
		s.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"feed-1", "feed-2", "B1", "登记序号 1", "同一条投料"} {
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

// 台账正常打开后文件才被改坏（feed-2 的结果序号改成 1）：下一次查询或
// 写入重新读取时必须返回 ErrCorruptData——即使访问的是另一个正常批次 B2
// 也不能绕过；被拒绝的重放不得返回第一条投料，被拒绝的新写入不留业务
// 记录、不占用请求编号。恢复后两个原请求分别取回各自的记录，被拒绝过的
// 新请求编号仍可正常使用并取得连续序号。
func TestFeedingRequestAliasingDetectedOnEveryReload(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	tm := feedingAliasBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 feed-2 的保存结果序号改坏，其余记录（含 B2）保持完整。
	bad := aliasFeedingResultToFirst(t, good)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B2"} {
		v, err := s.GetBatch(batchNo)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("损坏未修复时查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		}
		if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 用原编号重放：load 先于幂等重放执行，两个原请求都不能把第一条投料
	// 当成自己的登记返回。
	for _, reqNo := range []string{"feed-1", "feed-2"} {
		if _, err := s.AddFeeding(reqNo, "B1", "M1", "1.000", tm, "张三"); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("重放请求 %q 应先在重新读取时返回 ErrCorruptData，得到 %v", reqNo, err)
		}
	}
	// 对正常批次 B2 的幂等重放与新写入同样不能绕过。
	if _, err := s.AddFeeding("feed-b2", "B2", "M1", "1.000", tm, "张三"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上重放正常批次的请求应返回 ErrCorruptData，得到 %v", err)
	}
	if _, err := s.AddFeeding("feed-x", "B2", "M1", "1.000", tm, "钱七"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("损坏台账上写入正常批次应返回 ErrCorruptData，得到 %v", err)
	}

	// 被拒绝的访问不改动文件，新写入不留下请求记录。
	after, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("被拒绝的访问不应改动台账文件")
	}
	if bytes.Contains(after, []byte("feed-x")) {
		t.Fatalf("被拒绝的写入不应留下请求记录或占用请求编号")
	}

	// 恢复原文件后：两个原请求各自取回自己第一次成功的记录（序号 1、2），
	// 批次中两条投料都在；被拒绝过的新编号 feed-x 仍可提交，取得新序号。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("恢复后重放 feed-1 失败: %v", err)
	}
	checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
	r2, err := s.AddFeeding("feed-2", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("恢复后重放 feed-2 失败: %v", err)
	}
	checkFeedingView(t, r2, 2, "M1", "1", tm, "张三")
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("恢复后 B1 应为 2 条投料，得到 %d 条", len(view.Feedings))
	}
	// 换一个未使用的请求编号提交相同投料内容，仍是新的一次投料：取得
	// 连续序号 3，并计入实投量（M1 累计 3 克）。
	r3, err := s.AddFeeding("feed-x", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号恢复后应可正常使用: %v", err)
	}
	checkFeedingView(t, r3, 3, "M1", "1", tm, "张三")
	view, err = s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 3 {
		t.Fatalf("新编号相同内容应产生新投料，B1 应为 3 条，得到 %d 条", len(view.Feedings))
	}
	if got := view.Materials[0].ActualGrams; got != "3" {
		t.Fatalf("三条各 1 克投料后 M1 累计实投应为 3 克，得到 %q", got)
	}
}

// 不同批次可以各有序号为 1 的投料：即使投料内容完全相同，只要每个请求
// 指向各自批次的登记，就不是重复指向。正常台账必须可以打开、重放与查询。
func TestSameFeedingSeqInDifferentBatchesIsNotAliasing(t *testing.T) {
	dir := t.TempDir()
	setupFeedingAliasLedger(t, dir)
	tm := feedingAliasBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("不同批次各自序号 1 的正常台账应能打开: %v", err)
	}
	defer s.Close()

	r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("重放 feed-1 失败: %v", err)
	}
	checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
	rb2, err := s.AddFeeding("feed-b2", "B2", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("重放 feed-b2 失败: %v", err)
	}
	// B2 的序号 1 与 B1 的序号 1 互不影响。
	checkFeedingView(t, rb2, 1, "M1", "1", tm, "张三")
	b2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	if len(b2.Feedings) != 1 || b2.Feedings[0].Seq != 1 {
		t.Fatalf("B2 应只有自己的序号 1 投料，得到 %+v", b2.Feedings)
	}
}

// 批次关闭后，保存的投料请求仍受这项规则约束：B1 关闭后再把 feed-2 的
// 结果序号改成 1，打开台账仍必须以 ErrCorruptData 拒绝，关闭结果本身
// 完整也不能放行。
func TestAliasingRejectedEvenWhenBatchClosed(t *testing.T) {
	dir := t.TempDir()
	// 先建与 setupFeedingAliasLedger 相同的台账，但 B1 在两次投料后关闭。
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registerStandardRecipe(t, s, "recipe-1")
	if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBatch("s1", "B1"); err != nil {
		t.Fatal(err)
	}
	tm := feedingAliasBaseTime()
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-2", "B1", "M1", "1.000", tm, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseBatch("close-1", "B1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	bad := aliasFeedingResultToFirst(t, good)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if !errors.Is(err, ErrCorruptData) {
		t.Fatalf("关闭批次的两个投料请求指向同一条投料也应返回 ErrCorruptData，得到 %v", err)
	}
	if s2 != nil {
		s2.Close()
		t.Fatalf("损坏台账不应返回可用的 Store 对象")
	}
	msg := err.Error()
	for _, want := range []string{"feed-1", "feed-2", "B1", "登记序号 1"} {
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

// 已有投料没有对应请求记录时沿用现有读取规则：删除 feed-2 的请求记录后，
// B1 序号 2 的投料仍然保留、计入实投量，打开与查询正常，不要求补造请求。
// 跨请求核对只约束“请求之间不能指向同一条投料”，不要求每条投料都有请求。
func TestFeedingWithoutRequestRecordStillReadable(t *testing.T) {
	dir := t.TempDir()
	good := setupFeedingAliasLedger(t, dir)
	tm := feedingAliasBaseTime()

	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	delete(st.Requests, "feed-2")
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("投料缺少对应请求记录不应判为损坏: %v", err)
	}
	defer s.Close()

	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("删除请求记录不应删除投料，B1 应仍为 2 条，得到 %d 条", len(view.Feedings))
	}
	checkFeedingView(t, &view.Feedings[0], 1, "M1", "1", tm, "张三")
	checkFeedingView(t, &view.Feedings[1], 2, "M1", "1", tm, "张三")
	if got := view.Materials[0].ActualGrams; got != "2" {
		t.Fatalf("无请求记录的投料仍应计入实投量，M1 累计应为 2 克，得到 %q", got)
	}
	// feed-1 的幂等重放不受影响，仍取回序号 1；用未使用编号提交相同内容
	// 仍是新的一次投料（序号 3）。
	r1, err := s.AddFeeding("feed-1", "B1", "M1", "1.000", tm, "张三")
	if err != nil {
		t.Fatalf("feed-1 重放应成功: %v", err)
	}
	checkFeedingView(t, r1, 1, "M1", "1", tm, "张三")
}
