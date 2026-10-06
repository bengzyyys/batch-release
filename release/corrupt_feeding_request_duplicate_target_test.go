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

// 本文件是“两个不同的成功投料请求不能指向同一批次的同一条投料”的回归保障：
// 同一批次用两个不同请求编号登记内容完全相同的投料，应得到两个连续序号，
// 各请求的保存结果分别指向各自的序号。若把其中一个请求保存结果的序号改成
// 另一个请求的序号（其余内容保持一致），即使每个请求的内容与结果单独核对
// 都能通过、批次里的两条投料都还在且累计数量正确，打开台账及之后的每次
// 查询/写入重载都必须以 ErrCorruptData 拒绝整份台账——不能挑一个请求保留
// 后继续使用，不删除请求、不合并投料，也不替结果重新分配序号。

// setupDuplicateFeedingRequestLedger 在 dir 建立一份正常台账并关闭：
//   - B1（执行中）：feed-1 与 feed-2 两个不同请求编号登记了两条内容完全相同
//     的投料（M1 各 2 克、同一时刻、同一登记人），分别得到序号 1 与 2；
//   - B2（执行中）：feed-b2 登记一条内容相同的投料（M1 2 克，序号 1，
//     与 B1 各自编号，不属于重复对应）。
//
// 返回落盘后的台账文件内容，供各用例改坏后重写。
func setupDuplicateFeedingRequestLedger(t *testing.T, dir string) []byte {
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
	t1 := feedingRequestBaseTime()
	if _, err := s.AddFeeding("feed-1", "B1", "M1", "2", t1, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-2", "B1", "M1", "2", t1, "张三"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFeeding("feed-b2", "B2", "M1", "2", t1, "张三"); err != nil {
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

// corruptFeedingRequestSeq 把正常台账内容中 reqNo 请求的保存结果序号改成
// seq（其余内容保持一致），返回改写后的文件内容。
func corruptFeedingRequestSeq(t *testing.T, good []byte, reqNo string, seq int) []byte {
	t.Helper()
	var st persistedState
	if err := json.Unmarshal(good, &st); err != nil {
		t.Fatal(err)
	}
	req := st.Requests[reqNo]
	if req == nil {
		t.Fatalf("正常台账中应存在 %s 请求记录", reqNo)
	}
	var result FeedingView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		t.Fatal(err)
	}
	result.Seq = seq
	req.Result = marshalFeedingView(t, result)
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 两个不同请求编号登记内容完全相同的投料后，把第二个请求保存结果的序号改成
// 第一个请求的序号：此时每条请求的内容与结果单独核对都能通过（两条投料内容
// 相同），批次里的两条投料都在、累计数量也正确，但两个请求指向了同一条投料，
// Open 必须返回 ErrCorruptData——错误信息包含两个请求编号、批次编号与重复的
// 序号，不返回可用的台账对象，也不改写原文件。批次已关闭时同样适用。
func TestOpenRejectsFeedingRequestsPointingToSameFeeding(t *testing.T) {
	t.Run("执行中批次", func(t *testing.T) {
		dir := t.TempDir()
		good := setupDuplicateFeedingRequestLedger(t, dir)
		bad := corruptFeedingRequestSeq(t, good, "feed-2", 1)
		if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
			t.Fatal(err)
		}

		s, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("两个请求指向同一条投料应返回 ErrCorruptData，得到 %v", err)
		}
		if s != nil {
			s.Close()
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
	})

	t.Run("批次关闭后仍受约束", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("Open 失败: %v", err)
		}
		registerStandardRecipe(t, s, "recipe-1")
		if _, err := s.CreateBatch("b1", "B1", "R1", "v1", 10); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartBatch("s1", "B1"); err != nil {
			t.Fatal(err)
		}
		t1 := feedingRequestBaseTime()
		if _, err := s.AddFeeding("feed-1", "B1", "M1", "2", t1, "张三"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddFeeding("feed-2", "B1", "M1", "2", t1, "张三"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseBatch("c1", "B1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		good, err := os.ReadFile(filepath.Join(dir, stateFileName))
		if err != nil {
			t.Fatal(err)
		}
		bad := corruptFeedingRequestSeq(t, good, "feed-2", 1)
		if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
			t.Fatal(err)
		}

		s2, err := Open(dir)
		if !errors.Is(err, ErrCorruptData) {
			t.Fatalf("已关闭批次中两个请求指向同一条投料应返回 ErrCorruptData，得到 %v", err)
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
	})
}

// 台账正常打开后，保存内容被改成两个投料请求指向同一条投料：下一次查询或
// 写入必须返回 ErrCorruptData——即使本次访问的是另一个正常批次，也不能绕过；
// 被拒绝的写入不留业务记录、不占用请求编号，原台账内容不变。恢复后各请求
// 仍分别取回自己第一次成功的记录。
func TestFeedingRequestDuplicateTargetAfterOpenDetectedOnNextAccess(t *testing.T) {
	dir := t.TempDir()
	good := setupDuplicateFeedingRequestLedger(t, dir)
	t1 := feedingRequestBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 打开后把 feed-2 的保存结果序号改成 1，与 feed-1 指向同一条投料。
	bad := corruptFeedingRequestSeq(t, good, "feed-2", 1)
	if err := os.WriteFile(filepath.Join(dir, stateFileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// 查询损坏批次与正常批次都必须失败，不能沿用此前读到的内容。
	for _, batchNo := range []string{"B1", "B2"} {
		if v, err := s.GetBatch(batchNo); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("查询批次 %q 应返回 ErrCorruptData，得到 %v", batchNo, err)
		} else if v != nil {
			t.Fatalf("被拒绝的查询不应返回部分台账视图: %+v", v)
		}
	}
	// 对正常批次的新写入同样不能绕过。
	if _, err := s.AddFeeding("feed-new", "B2", "M1", "1", t1, "赵六"); !errors.Is(err, ErrCorruptData) {
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
	if bytes.Contains(after, []byte("feed-new")) {
		t.Fatalf("被拒绝的写入不应留下请求记录")
	}

	// 恢复后：两个请求仍分别取回各自第一次成功的记录，被拒绝过的请求
	// 编号可以正常使用并取得连续序号。
	if err := os.WriteFile(filepath.Join(dir, stateFileName), good, 0o644); err != nil {
		t.Fatal(err)
	}
	replay1, err := s.AddFeeding("feed-1", "B1", "M1", "2", t1, "张三")
	if err != nil {
		t.Fatalf("恢复后重放 feed-1 失败: %v", err)
	}
	checkFeedingView(t, replay1, 1, "M1", "2", t1, "张三")
	replay2, err := s.AddFeeding("feed-2", "B1", "M1", "2", t1, "张三")
	if err != nil {
		t.Fatalf("恢复后重放 feed-2 失败: %v", err)
	}
	checkFeedingView(t, replay2, 2, "M1", "2", t1, "张三")
	f, err := s.AddFeeding("feed-new", "B2", "M1", "1", t1, "赵六")
	if err != nil {
		t.Fatalf("被拒绝过的请求编号应仍可合法提交: %v", err)
	}
	if f.Seq != 2 || f.Grams != "1" {
		t.Fatalf("新投料结果不正确: %+v", f)
	}
}

// 正常使用不受这项规则影响：同一请求编号和原内容重复提交只取回第一次成功
// 的记录；换一个未使用的请求编号提交相同投料内容，仍表示新的一次投料，
// 两条记录分别保留自己的序号并各自计入实投量；不同批次各自有序号为 1 的
// 投料（即使内容相同）不属于重复对应。
func TestDistinctFeedingRequestsKeepOwnSeqs(t *testing.T) {
	dir := t.TempDir()
	setupDuplicateFeedingRequestLedger(t, dir)
	t1 := feedingRequestBaseTime()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("正常台账应能打开: %v", err)
	}
	defer s.Close()

	// 同编号同内容重放：取回第一次的记录，不追加投料。
	replay, err := s.AddFeeding("feed-1", "B1", "M1", "2", t1, "张三")
	if err != nil {
		t.Fatalf("重放应成功: %v", err)
	}
	checkFeedingView(t, replay, 1, "M1", "2", t1, "张三")

	// 两个请求的两条投料分别保留自己的序号，并各自计入实投量。
	view, err := s.GetBatch("B1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Feedings) != 2 {
		t.Fatalf("B1 应为 2 条投料，得到 %d 条", len(view.Feedings))
	}
	checkFeedingView(t, &view.Feedings[0], 1, "M1", "2", t1, "张三")
	checkFeedingView(t, &view.Feedings[1], 2, "M1", "2", t1, "张三")
	for _, m := range view.Materials {
		if m.MaterialNo == "M1" && m.ActualGrams != "4" {
			t.Fatalf("M1 累计实投应为 4 克，得到 %q", m.ActualGrams)
		}
	}

	// 不同批次各自有序号为 1 的投料，内容相同也不属于重复对应。
	view2, err := s.GetBatch("B2")
	if err != nil {
		t.Fatal(err)
	}
	if len(view2.Feedings) != 1 || view2.Feedings[0].Seq != 1 {
		t.Fatalf("B2 应有 1 条序号为 1 的投料: %+v", view2.Feedings)
	}

	// 新请求编号提交相同内容仍是新的一次投料，取得连续序号。
	f, err := s.AddFeeding("feed-3", "B1", "M1", "2", t1, "张三")
	if err != nil {
		t.Fatalf("新请求编号提交相同内容应成功: %v", err)
	}
	checkFeedingView(t, f, 3, "M1", "2", t1, "张三")
}
