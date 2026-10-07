package release

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件是“关闭批次与追加投料同时提交”这一业务边界的回归保障。
// 两个调用方各自打开同一个本地台账（两个独立的 Store 对象），对同一执行中
// 的批次，一方提交关闭、另一方用尚未使用的请求编号追加一条合法投料；两方在
// 提交前都查询过批次记录。回归点：任何一方都不能凭自己内存中的旧记录，把另
// 一方已经提交成功的业务变更覆盖掉。
//
// 同时提交允许两种完整结果，不规定哪一方先被接受：
//   - 投料先被接受：投料与关闭都成功，关闭返回的批次包含原有投料和这条新增
//     投料，新增记录沿用批次的连续登记序号，物料、克数、时间、登记人完整保留；
//   - 关闭先被接受：关闭成功，投料返回 ErrInvalidState，关闭结果只包含关闭前
//     已确认的投料；被拒绝的投料不出现在记录中，不增加实投量，也不留下空的
//     登记位置。
//
// 两种结果都要保护数量核对与投料记录的对应关系：关闭结果仍采用原先绑定的
// 配方版本与计划份数，应投量不变，实投量只累计实际接受的投料，差额仍为实投
// 减应投；配方内尚未投料的物料仍须列出、实投为零。数量不足仍允许关闭，关闭
// 不是数量放行检查。两个操作都返回后，任一调用方查询到的批次都必须已关闭，
// 并与成功关闭时返回的投料列表和逐物料核对一致。
//
// 本文件只补充现有功能的自动化检查：保留现有公开入口、错误类别与关闭规则，
// 不增加新的业务操作。
//
// 复用 closeFeedTime（close_batch_replay_test.go）、materialsMap 与
// checkRequirement（draft_adjust_regression_test.go）、mustGetBatch
// （start_batch_replay_test.go）。
//
// 场景固定为：配方 RCF/v1（M1 每份 2 克、M2 每份 1 克），批次 B1 计划 5 份
// （应投 M1=10、M2=5），关闭前已投 M1 4 克（序号 1，登记人张三）；并发追加
// 的投料为 M1 2 克（请求编号 f2，登记人李四）。投料被接受时关闭结果为
// M1 实投 6、差额 -4；被拒绝时为实投 4、差额 -6。M2 从未投料，两种结果中
// 都必须列出（应投 5、实投 0、差额 -5）。

// concurrentFeedTime 是并发追加投料（f2）的固定填写时间，与原有投料（f1）
// 的时间区分开，用于验证新增记录的时间被完整保留。
func concurrentFeedTime() time.Time {
	return closeFeedTime().Add(time.Hour)
}

// setupConcurrentCloseLedger 在 dir 建立并发场景台账：登记 RCF/v1，创建
// B1（5 份），开始执行，并登记原有投料 f1（M1 4 克，张三）。返回前关闭
// Store，之后的操作由两个调用方各自打开的 Store 完成。
func setupConcurrentCloseLedger(t *testing.T, dir string) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := s.RegisterRecipe("recipe-rcf", "RCF", "v1", "并发关闭配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "2"},
		{MaterialNo: "M2", Grams: "1"},
	}); err != nil {
		t.Fatalf("登记 RCF/v1 失败: %v", err)
	}
	if _, err := s.CreateBatch("b1", "B1", "RCF", "v1", 5); err != nil {
		t.Fatalf("创建批次失败: %v", err)
	}
	if _, err := s.StartBatch("start-b1", "B1"); err != nil {
		t.Fatalf("开始执行失败: %v", err)
	}
	if _, err := s.AddFeeding("f1", "B1", "M1", "4", closeFeedTime(), "张三"); err != nil {
		t.Fatalf("登记原有投料失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭台账失败: %v", err)
	}
}

// openConcurrentPair 模拟两个调用方：各自打开同一台账并查询过批次记录
// （确认批次执行中、只有原有投料 f1），返回两个独立的 Store。此后的关闭与
// 投料分别通过这两个对象提交，任何一方都不能凭这份旧查询覆盖对方的变更。
func openConcurrentPair(t *testing.T, dir string) (closer, feeder *Store) {
	t.Helper()
	closer, err := Open(dir)
	if err != nil {
		t.Fatalf("关闭方 Open 失败: %v", err)
	}
	feeder, err = Open(dir)
	if err != nil {
		closer.Close()
		t.Fatalf("投料方 Open 失败: %v", err)
	}
	for i, s := range []*Store{closer, feeder} {
		v, err := s.GetBatch("B1")
		if err != nil {
			t.Fatalf("调用方 %d 提交前查询批次失败: %v", i+1, err)
		}
		if v.Status != StatusExecuting || len(v.Feedings) != 1 {
			t.Fatalf("提交前批次应为执行中且只有原有投料，得到 状态=%s 投料=%d 条",
				v.Status, len(v.Feedings))
		}
	}
	return closer, feeder
}

// wantConcurrentFeedings 给出关闭结果中应保留的投料列表：fed 为 true 表示
// 并发投料被接受（原有 f1 加新增 f2，序号 1、2 连续），否则只有原有 f1。
func wantConcurrentFeedings(fed bool) []FeedingView {
	want := []FeedingView{
		{Seq: 1, MaterialNo: "M1", Grams: "4", Time: closeFeedTime(), Registrar: "张三"},
	}
	if fed {
		want = append(want, FeedingView{
			Seq: 2, MaterialNo: "M1", Grams: "2", Time: concurrentFeedTime(), Registrar: "李四",
		})
	}
	return want
}

// checkConcurrentClosedView 校验并发场景下已关闭批次 B1 的完整视图（关闭的
// 返回结果或随后按批次查询的结果都适用）：保留绑定的 RCF/v1 与计划份数 5，
// 状态为已关闭，投料按登记顺序完整保留（fed 表示并发投料是否被接受），
// 逐物料核对与投料记录对应——应投量不变（M1=10、M2=5），实投量只累计实际
// 接受的投料，差额为实投减应投；从未投料的 M2 仍列出、实投为零。
func checkConcurrentClosedView(t *testing.T, v *BatchView, fed bool) {
	t.Helper()
	if v.BatchNo != "B1" {
		t.Fatalf("批次编号应为 B1，得到 %q", v.BatchNo)
	}
	if v.RecipeNo != "RCF" || v.RecipeVersion != "v1" || v.RecipeName != "并发关闭配方" {
		t.Fatalf("应保留实际绑定的 RCF/v1（并发关闭配方），得到 %q %q %q",
			v.RecipeNo, v.RecipeVersion, v.RecipeName)
	}
	if v.PlannedPortions != 5 {
		t.Fatalf("计划份数应仍为 5，得到 %d", v.PlannedPortions)
	}
	if v.Status != StatusClosed {
		t.Fatalf("状态应为已关闭，得到 %s", v.Status)
	}
	checkConcurrentFeedingList(t, v.Feedings, wantConcurrentFeedings(fed))
	mats := materialsMap(v)
	if len(mats) != 2 {
		t.Fatalf("应只有 M1、M2 两种物料，得到 %d 项: %+v", len(mats), v.Materials)
	}
	if fed {
		// 投料被接受：实投 4+2=6，差额 6-10=-4。
		checkRequirement(t, mats, "M1", "10", "6", "-4")
	} else {
		// 投料被拒绝：实投仍为 4，差额仍为 -6，不留下任何痕迹。
		checkRequirement(t, mats, "M1", "10", "4", "-6")
	}
	// 从未投料的 M2 不能因本次关闭被省略：应投 5、实投 0、差额 -5。
	checkRequirement(t, mats, "M2", "5", "0", "-5")
}

// checkConcurrentFeedingList 逐条核对投料列表：条数一致（被拒绝的投料不能
// 多出一条，也不能留下空的登记位置），序号、物料、克数、时间、登记人逐项
// 相符，按登记顺序排列。
func checkConcurrentFeedingList(t *testing.T, got, want []FeedingView) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("投料应为 %d 条（被拒绝的投料不能出现，也不能留空位），得到 %d 条: %+v",
			len(want), len(got), got)
	}
	for i, w := range want {
		f := got[i]
		if f.Seq != w.Seq || f.MaterialNo != w.MaterialNo || f.Grams != w.Grams ||
			!f.Time.Equal(w.Time) || f.Registrar != w.Registrar {
			t.Fatalf("第 %d 条投料应为 %+v，得到 %+v", i+1, w, f)
		}
	}
}

// 投料先被接受（两个调用方各自打开台账、都查询过旧记录之后，投料方先提交
// 成功）：随后关闭方的提交不能凭自己内存中的旧记录覆盖这条已成功的投料——
// 关闭必须成功，且关闭结果包含原有投料与新增投料（序号 1、2 连续，物料、
// 克数、时间、登记人完整保留），M1 实投 6、差额 -4，M2 仍列出、实投为零。
// 两个操作都返回后，任一调用方查询到的批次都已关闭并与关闭结果一致。
func TestConcurrentCloseAndFeedingFeedingAcceptedFirst(t *testing.T) {
	dir := t.TempDir()
	setupConcurrentCloseLedger(t, dir)
	closer, feeder := openConcurrentPair(t, dir)
	defer closer.Close()
	defer feeder.Close()

	// 投料方先提交：合法投料（绑定版本物料、数量未超上限、请求编号未使用）
	// 必须成功，序号为批次内的连续登记序号 2。
	fed, err := feeder.AddFeeding("f2", "B1", "M1", "2", concurrentFeedTime(), "李四")
	if err != nil {
		t.Fatalf("合法投料应成功: %v", err)
	}
	wantFed := FeedingView{Seq: 2, MaterialNo: "M1", Grams: "2", Time: concurrentFeedTime(), Registrar: "李四"}
	if fed.Seq != wantFed.Seq || fed.MaterialNo != wantFed.MaterialNo || fed.Grams != wantFed.Grams ||
		!fed.Time.Equal(wantFed.Time) || fed.Registrar != wantFed.Registrar {
		t.Fatalf("投料结果应为 %+v，得到 %+v", wantFed, *fed)
	}

	// 关闭方此前查询到的是没有 f2 的旧记录，但其提交不能覆盖投料方已成功
	// 的变更：关闭成功，结果包含两条投料。
	closed, err := closer.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("投料已被接受后关闭应成功: %v", err)
	}
	checkConcurrentClosedView(t, closed, true)

	// 两个操作都返回后，任一调用方查询到的批次都已关闭，投料列表与逐物料
	// 核对同成功关闭时返回的结果一致。
	checkConcurrentClosedView(t, mustGetBatch(t, closer, "B1"), true)
	checkConcurrentClosedView(t, mustGetBatch(t, feeder, "B1"), true)

	// 已接受的投料请求在批次关闭后仍可重放出首次成功的结果，说明这条投料
	// 确实登记在案，而不是只在返回时临时出现。
	replay, err := feeder.AddFeeding("f2", "B1", "M1", "2", concurrentFeedTime(), "李四")
	if err != nil {
		t.Fatalf("批次关闭后重放已接受的投料请求应成功: %v", err)
	}
	if replay.Seq != 2 || replay.Grams != "2" || replay.Registrar != "李四" {
		t.Fatalf("重放应返回首次成功的投料结果，得到 %+v", *replay)
	}
}

// 关闭先被接受（两个调用方各自打开台账、都查询过旧记录之后，关闭方先提交
// 成功）：随后投料方的提交返回 ErrInvalidState；关闭结果只包含关闭前已确认
// 的投料，被拒绝的投料不出现在记录中、不增加实投量、不留下空的登记位置。
// M1 实投仍为 4、差额仍为 -6；M2 仍列出、实投为零。数量不足仍允许关闭，
// 关闭没有被改成数量放行检查。两个操作都返回后，任一调用方查询到的批次都
// 已关闭并与关闭结果一致。
func TestConcurrentCloseAndFeedingCloseAcceptedFirst(t *testing.T) {
	dir := t.TempDir()
	setupConcurrentCloseLedger(t, dir)
	closer, feeder := openConcurrentPair(t, dir)
	defer closer.Close()
	defer feeder.Close()

	// 关闭方先提交：批次执行中、数量尚未吻合（M1 欠投 6 克、M2 未投），
	// 关闭仍应成功——关闭只确认已有投料，不要求数量吻合。
	closed, err := closer.CloseBatch("close-b1", "B1")
	if err != nil {
		t.Fatalf("数量不足的执行中批次应可关闭: %v", err)
	}
	checkConcurrentClosedView(t, closed, false)

	// 投料方此前查询到的是执行中的旧记录，但批次已关闭：投料必须返回
	// ErrInvalidState，不能凭旧记录把投料追加进已关闭的批次。
	if _, err := feeder.AddFeeding("f2", "B1", "M1", "2", concurrentFeedTime(), "李四"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("批次已关闭后投料应返回 ErrInvalidState，得到 %v", err)
	}

	// 两个操作都返回后，任一调用方查询到的批次都已关闭，且与关闭结果一致：
	// 只有原有投料（序号 1），没有被拒绝的投料，也没有空的登记位置。
	checkConcurrentClosedView(t, mustGetBatch(t, closer, "B1"), false)
	checkConcurrentClosedView(t, mustGetBatch(t, feeder, "B1"), false)
}

// 关闭与投料真正同时提交（两个调用方各自打开台账、都查询过旧记录，两个
// 操作并发发出）：不规定哪一方先被接受，但结果必须是两种完整结果之一——
//   - 投料被接受：投料与关闭都成功，关闭结果包含两条投料，M1 实投 6、
//     差额 -4；
//   - 关闭先被接受：关闭成功，投料返回 ErrInvalidState，关闭结果只含原有
//     投料，M1 实投 4、差额 -6。
// 无论哪种结果，关闭都必须成功（关闭不能被改成数量放行检查，也不能因并发
// 投料而失败），投料只允许“完整成功”或“ErrInvalidState”两种结局；两个
// 操作都返回后，任一调用方查询到的批次都已关闭，并与成功关闭时返回的投料
// 列表和逐物料核对一致——不能出现投料报告成功却从关闭结果或台账中消失，
// 也不能出现关闭结果未包含某条投料而最终台账又多出它。
func TestConcurrentCloseAndFeedingEitherOutcome(t *testing.T) {
	const rounds = 20
	outcomes := map[bool]int{} // fed → 出现次数，仅用于日志
	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		setupConcurrentCloseLedger(t, dir)
		closer, feeder := openConcurrentPair(t, dir)

		type closeResult struct {
			view *BatchView
			err  error
		}
		type feedResult struct {
			view *FeedingView
			err  error
		}
		start := make(chan struct{})
		closeCh := make(chan closeResult, 1)
		feedCh := make(chan feedResult, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			v, err := closer.CloseBatch("close-b1", "B1")
			closeCh <- closeResult{v, err}
		}()
		go func() {
			defer wg.Done()
			<-start
			v, err := feeder.AddFeeding("f2", "B1", "M1", "2", concurrentFeedTime(), "李四")
			feedCh <- feedResult{v, err}
		}()
		close(start)
		wg.Wait()

		cres, fres := <-closeCh, <-feedCh

		// 关闭必须成功：数量不足不是拒绝理由，并发投料也不是。
		if cres.err != nil {
			t.Fatalf("第 %d 轮：并发提交下关闭应成功，得到 %v", round+1, cres.err)
		}

		// 投料只有两种合法结局：完整成功，或 ErrInvalidState。
		fed := fres.err == nil
		if !fed && !errors.Is(fres.err, ErrInvalidState) {
			t.Fatalf("第 %d 轮：投料只应成功或返回 ErrInvalidState，得到 %v", round+1, fres.err)
		}
		if fed {
			// 报告成功的投料必须完整保留物料、克数、时间与登记人，
			// 并沿用批次的连续登记序号 2。
			want := FeedingView{Seq: 2, MaterialNo: "M1", Grams: "2", Time: concurrentFeedTime(), Registrar: "李四"}
			if fres.view == nil || fres.view.Seq != want.Seq || fres.view.MaterialNo != want.MaterialNo ||
				fres.view.Grams != want.Grams || !fres.view.Time.Equal(want.Time) || fres.view.Registrar != want.Registrar {
				t.Fatalf("第 %d 轮：投料结果应为 %+v，得到 %+v", round+1, want, fres.view)
			}
		}
		outcomes[fed]++

		// 关闭结果必须与投料的结局对应：投料被接受时包含两条投料，
		// 被拒绝时只含原有投料——不能报告成功却从关闭结果中消失，
		// 也不能被拒绝却出现在关闭结果中。
		checkConcurrentClosedView(t, cres.view, fed)

		// 两个操作都返回后，任一调用方查询到的批次都已关闭，并与成功
		// 关闭时返回的投料列表和逐物料核对一致（每次查询都重新读取台账，
		// 这也同时验证了落盘状态本身完好、没有留下空的登记位置）。
		checkConcurrentClosedView(t, mustGetBatch(t, closer, "B1"), fed)
		checkConcurrentClosedView(t, mustGetBatch(t, feeder, "B1"), fed)

		closer.Close()
		feeder.Close()
	}
	t.Logf("两种完整结果出现次数：投料被接受 %d 轮，投料被拒绝 %d 轮",
		outcomes[true], outcomes[false])
}
