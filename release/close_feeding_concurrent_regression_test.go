package release

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件为“关闭批次”（CloseBatch）与“追加投料”（AddFeeding）在两个调用方
// 同时提交时的业务边界补充回归保障。场景固定为：
//   - 两个调用方各自 Open 同一个本地台账（两个独立的 Store 句柄），且都在
//     提交前查询并取得过同一执行中批次的记录；
//   - 一方提交关闭，另一方用不同的、尚未使用过的请求编号追加一条合法投料
//     （物料属于批次绑定的配方版本、数量不超上限）；
//   - 批次此前已有投料，配方版本与计划份数在开始执行时固定。
//
// 现有实现通过进程内互斥与文件锁（flock）把写入串行化，并在拿到锁之后
// 重新读取台账：先提交的一方成功，后提交的一方必须在最新状态上判定，不能
// 用自己提交前看到的旧快照覆盖另一方已经落盘的业务变更。因此允许两种完整
// 结果，不规定哪一方必须先成功：
//
//  1. 投料先被接受：投料与关闭都成功。关闭返回的批次同时包含原有投料与
//     新增投料；新增记录沿用批次连续的登记序号，物料编号、克数、投料时间、
//     登记人完整保留。
//  2. 关闭先被接受：关闭成功，投料返回 ErrInvalidState。关闭结果只包含
//     关闭前已确认的投料；被拒绝的投料不出现在记录中，不增加实投量，也不
//     留下空的登记位置；失败不占用请求编号。
//
// 两种结果都必须保住数量核对与投料记录的对应关系：关闭仍采用批次绑定的
// 配方版本与计划份数，应投量不变，实投量只累计实际接受的投料，差额恒为
// “实投减应投”；配方内尚未投料的另一种物料仍须列出（实投为零）。数量不足
// 不阻止关闭——关闭不是数量放行检查。两个操作都返回后，任一句柄（含重新
// 打开的新句柄）查到的批次都必须已关闭，并与成功关闭时返回的投料列表和
// 逐物料核对一致：投料不能报告成功却从关闭结果或台账中消失，关闭结果也不能
// 漏掉某条投料而最终台账里又多出它。
//
// 本文件只补充针对该边界的自动化检查，保留现有公开入口、错误类别与关闭
// 规则，不增加新的业务操作。
//
// 台账数据（各用例独立建账）：
//   - 配方 RX/v1“并发核对配方”：M1 每份 10 克、M2 每份 7 克；
//   - 批次 B1 计划 1 份：M1 应投 10 克、M2 应投 7 克；
//   - 开始执行后已有一条投料：M1 4 克（请求 f-orig，序号 1，登记人张三）；
//   - 并发追加的投料：M1 2 克（请求 f-new，成功时序号应为 2，登记人李四）。
//
// 于是数量核对恰好覆盖题述边界：M1 应投 10、原实投 4；追加被接受时关闭
// 结果为实投 6、差额 -4，追加被拒绝时仍为实投 4、差额 -6；M2 始终未投料，
// 实投 0、差额 -7，且不能在关闭结果中被省略。

const (
	concurrentRecipeReq  = "recipe-rx"
	concurrentCreateReq  = "b1"
	concurrentStartReq   = "start-b1"
	concurrentOrigFeedNo = "f-orig"
	concurrentNewFeedNo  = "f-new"
	concurrentCloseReq   = "close-b1"
)

// concurrentOriginalTime 是关闭前已有投料（序号 1）填写的时间。
func concurrentOriginalTime() time.Time {
	return time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
}

// concurrentNewTime 是并发追加投料填写的时间，与登记先后无关，只用于核对
// 内容是否完整保留。
func concurrentNewTime() time.Time {
	return time.Date(2026, 10, 7, 10, 30, 0, 0, time.UTC)
}

// setupConcurrentCloseFeedingLedger 在 dir 建立本文件共用的起始台账：
// RX/v1（M1=10、M2=7），B1 计划 1 份，开始执行后登记一条 M1 4 克的投料
// （f-orig，序号 1）。返回时批次仍为执行中、尚未关闭，初始句柄已关闭，
// 供用例另行打开两个句柄模拟两个调用方。
func setupConcurrentCloseFeedingLedger(t *testing.T, dir string) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 起始台账失败: %v", err)
	}
	if _, err := s.RegisterRecipe(concurrentRecipeReq, "RX", "v1", "并发核对配方", []MaterialInput{
		{MaterialNo: "M1", Grams: "10"},
		{MaterialNo: "M2", Grams: "7"},
	}); err != nil {
		t.Fatalf("登记 RX/v1 失败: %v", err)
	}
	if _, err := s.CreateBatch(concurrentCreateReq, "B1", "RX", "v1", 1); err != nil {
		t.Fatalf("创建批次 B1 失败: %v", err)
	}
	if _, err := s.StartBatch(concurrentStartReq, "B1"); err != nil {
		t.Fatalf("开始执行 B1 失败: %v", err)
	}
	if _, err := s.AddFeeding(concurrentOrigFeedNo, "B1", "M1", "4",
		concurrentOriginalTime(), "张三"); err != nil {
		t.Fatalf("登记原有投料失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭起始句柄失败: %v", err)
	}
}

// openTwoConcurrentHandles 在同一目录上打开两个彼此独立的台账句柄，模拟
// 两个调用方分别打开同一个本地台账。
func openTwoConcurrentHandles(t *testing.T, dir string) (closer, feeder *Store) {
	t.Helper()
	closer, err := Open(dir)
	if err != nil {
		t.Fatalf("调用方 A 打开台账失败: %v", err)
	}
	t.Cleanup(func() { closer.Close() })
	feeder, err = Open(dir)
	if err != nil {
		t.Fatalf("调用方 B 打开台账失败: %v", err)
	}
	t.Cleanup(func() { feeder.Close() })
	return closer, feeder
}

// assertBothSawExecutingBatch 校验两个调用方在提交前都取得过同一条执行中
// 批次记录：状态执行中、只有原有一条投料、M1 实投 4。两方都带着这份旧
// 快照进入提交，后续成功与否不能以这份旧快照为准。
func assertBothSawExecutingBatch(t *testing.T, closer, feeder *Store) {
	t.Helper()
	for name, s := range map[string]*Store{"关闭方": closer, "投料方": feeder} {
		v, err := s.GetBatch("B1")
		if err != nil {
			t.Fatalf("%s 提交前查询批次失败: %v", name, err)
		}
		if v.Status != StatusExecuting {
			t.Fatalf("%s 提交前批次应为执行中，得到 %s", name, v.Status)
		}
		if len(v.Feedings) != 1 || v.Feedings[0].Seq != 1 ||
			v.Feedings[0].MaterialNo != "M1" || v.Feedings[0].Grams != "4" {
			t.Fatalf("%s 提交前应只看到原有 4 克投料，得到 %+v", name, v.Feedings)
		}
		mats := materialsMap(v)
		checkRequirement(t, mats, "M1", "10", "4", "-6")
		checkRequirement(t, mats, "M2", "7", "0", "-7")
	}
}

// assertClosedIdentity 校验关闭结果仍采用批次绑定的 RX/v1 与计划 1 份、
// 状态为已关闭——并发提交不能改掉配方版本、计划份数，也不能把数量不足
// 变成关闭的拦截条件。
func assertClosedIdentity(t *testing.T, v *BatchView) {
	t.Helper()
	if v.BatchNo != "B1" || v.RecipeNo != "RX" || v.RecipeVersion != "v1" ||
		v.RecipeName != "并发核对配方" || v.PlannedPortions != 1 {
		t.Fatalf("关闭结果应保留绑定的 RX/v1、1 份，得到 %+v", v)
	}
	if v.Status != StatusClosed {
		t.Fatalf("关闭结果状态应为已关闭，得到 %s", v.Status)
	}
}

// assertFeedingRecord 核对一条投料记录的完整内容：序号、物料、克数、
// 投料时间（同一时刻）与登记人逐项一致。
func assertFeedingRecord(t *testing.T, f FeedingView, seq int, material, grams string, tm time.Time, registrar string) {
	t.Helper()
	if f.Seq != seq || f.MaterialNo != material || f.Grams != grams ||
		!f.Time.Equal(tm) || f.Registrar != registrar {
		t.Fatalf("序号 %d 的投料应为 物料=%s 克数=%s 时间=%v 登记人=%s，得到 %+v",
			seq, material, grams, tm, registrar, f)
	}
}

// assertReconciliation 按“关闭时实际接受的投料”逐物料核对：M1 应投 10
// 不变，M2 应投 7 不变且始终列出；实投只累计实际接受的投料，差额恒为
// 实投减应投。m1Actual 取 "6"（追加被接受）或 "4"（追加被拒绝）。
func assertReconciliation(t *testing.T, v *BatchView, m1Actual, m1Diff string) {
	t.Helper()
	mats := materialsMap(v)
	if len(mats) != 2 {
		t.Fatalf("配方内两种物料都必须列出（含未投料的 M2），得到 %d 项: %+v",
			len(mats), v.Materials)
	}
	checkRequirement(t, mats, "M1", "10", m1Actual, m1Diff)
	checkRequirement(t, mats, "M2", "7", "0", "-7")
}

// assertSameClosedView 校验任一句柄查到的批次都与成功关闭时返回的结果
// 一致：状态、投料列表（条数、顺序、内容）与逐物料核对逐项相同。
func assertSameClosedView(t *testing.T, got, want *BatchView) {
	t.Helper()
	assertClosedIdentity(t, got)
	if len(got.Feedings) != len(want.Feedings) {
		t.Fatalf("查询到 %d 条投料，关闭结果为 %d 条，两者必须一致: got=%+v want=%+v",
			len(got.Feedings), len(want.Feedings), got.Feedings, want.Feedings)
	}
	for i := range want.Feedings {
		gf, wf := got.Feedings[i], want.Feedings[i]
		if gf.Seq != wf.Seq || gf.MaterialNo != wf.MaterialNo || gf.Grams != wf.Grams ||
			!gf.Time.Equal(wf.Time) || gf.Registrar != wf.Registrar {
			t.Fatalf("第 %d 条投料查询与关闭结果不一致: got=%+v want=%+v", i+1, gf, wf)
		}
	}
	gm, wm := materialsMap(got), materialsMap(want)
	for _, no := range []string{"M1", "M2"} {
		g, ok1 := gm[no]
		w, ok2 := wm[no]
		if !ok1 || !ok2 {
			t.Fatalf("查询缺少物料 %s 的核对项: got=%+v", no, got.Materials)
		}
		if g != w {
			t.Fatalf("物料 %s 的数量核对查询与关闭结果不一致: got=%+v want=%+v", no, g, w)
		}
	}
}

// assertClosedAfterConcurrent 是两种完整结果共用的收尾核对：
//   - 两个调用方随后查询到的批次都已关闭，且与关闭返回结果一致；
//   - 用原关闭请求编号重放，任一句柄都取回同一份首次关闭结果，不产生
//     新的业务变更；
//   - 重新打开台账（触发全部读取期一致性校验）成功，查询与关闭重放仍与
//     首次关闭结果一致——报告成功的投料不会在落盘后消失，被拒绝的投料
//     也不会在重开后多出。
func assertClosedAfterConcurrent(t *testing.T, dir string, closer, feeder *Store, closed *BatchView) {
	t.Helper()
	assertSameClosedView(t, mustGetBatch(t, closer, "B1"), closed)
	assertSameClosedView(t, mustGetBatch(t, feeder, "B1"), closed)

	replayFromCloser, err := closer.CloseBatch(concurrentCloseReq, "B1")
	if err != nil {
		t.Fatalf("关闭方重放关闭请求应成功: %v", err)
	}
	assertSameClosedView(t, replayFromCloser, closed)
	replayFromFeeder, err := feeder.CloseBatch(concurrentCloseReq, "B1")
	if err != nil {
		t.Fatalf("投料方重放关闭请求应成功: %v", err)
	}
	assertSameClosedView(t, replayFromFeeder, closed)

	// 重放不改变业务结果：两边再查仍是同一份已关闭批次。
	assertSameClosedView(t, mustGetBatch(t, closer, "B1"), closed)
	assertSameClosedView(t, mustGetBatch(t, feeder, "B1"), closed)

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("并发提交后的台账重新打开应成功（保存结果须与实际记录一致）: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	assertSameClosedView(t, mustGetBatch(t, reopened, "B1"), closed)
	replayAfterReopen, err := reopened.CloseBatch(concurrentCloseReq, "B1")
	if err != nil {
		t.Fatalf("重新打开后重放关闭请求应成功: %v", err)
	}
	assertSameClosedView(t, replayAfterReopen, closed)
}

// TestCloseAndFeedingConcurrentAcceptsEitherOutcome 让两个调用方在同一启动
// 屏障后真正并发提交关闭与追加投料。文件锁决定先后，两种完整结果都允许，
// 但每种结果都必须满足全部一致性约束。重复多轮以覆盖两种先后次序，并统计
// 两种结果实际出现的次数（仅记录，不强制某一种必须出现——先后次序由调度
// 决定；两种次序各自另有确定性测试钉死）。
func TestCloseAndFeedingConcurrentAcceptsEitherOutcome(t *testing.T) {
	const rounds = 30
	var feedingFirst, closeFirst int
	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		setupConcurrentCloseFeedingLedger(t, dir)
		closer, feeder := openTwoConcurrentHandles(t, dir)
		assertBothSawExecutingBatch(t, closer, feeder)

		start := make(chan struct{})
		var wg sync.WaitGroup
		type closeResult struct {
			view *BatchView
			err  error
		}
		type feedResult struct {
			view *FeedingView
			err  error
		}
		closeRes := make(chan closeResult, 1)
		feedRes := make(chan feedResult, 1)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			v, err := closer.CloseBatch(concurrentCloseReq, "B1")
			closeRes <- closeResult{view: v, err: err}
		}()
		go func() {
			defer wg.Done()
			<-start
			v, err := feeder.AddFeeding(concurrentNewFeedNo, "B1", "M1", "2",
				concurrentNewTime(), "李四")
			feedRes <- feedResult{view: v, err: err}
		}()
		close(start)
		wg.Wait()
		cr, fr := <-closeRes, <-feedRes

		// 关闭在两种完整结果里都必须成功：数量不足（M1 欠投、M2 未投）不
		// 阻止关闭，关闭不是数量放行检查。
		if cr.err != nil {
			t.Fatalf("第 %d 轮：并发提交中关闭应成功，得到 %v", round+1, cr.err)
		}
		if cr.view == nil {
			t.Fatalf("第 %d 轮：关闭成功应返回批次结果", round+1)
		}
		assertClosedIdentity(t, cr.view)

		if fr.err == nil {
			// 结果一：投料先被接受。投料与关闭都成功。
			feedingFirst++
			if fr.view == nil {
				t.Fatalf("第 %d 轮：投料成功应返回投料结果", round+1)
			}
			// 新增记录沿用批次连续登记序号（原有为 1，本次必须是 2），
			// 物料、克数、时间、登记人完整保留。
			assertFeedingRecord(t, *fr.view, 2, "M1", "2", concurrentNewTime(), "李四")

			// 关闭结果须同时包含原有投料与新增投料，按登记顺序排列。
			if len(cr.view.Feedings) != 2 {
				t.Fatalf("第 %d 轮：投料先成功时关闭结果应包含 2 条投料，得到 %d 条 %+v",
					round+1, len(cr.view.Feedings), cr.view.Feedings)
			}
			assertFeedingRecord(t, cr.view.Feedings[0], 1, "M1", "4", concurrentOriginalTime(), "张三")
			assertFeedingRecord(t, cr.view.Feedings[1], 2, "M1", "2", concurrentNewTime(), "李四")
			// 实投只累计实际接受的投料：4+2=6，差额 6-10=-4。
			assertReconciliation(t, cr.view, "6", "-4")

			// 投料方重放原请求：取回首次成功的序号 2 结果，不新增投料。
			replayFeed, err := feeder.AddFeeding(concurrentNewFeedNo, "B1", "M1", "2",
				concurrentNewTime(), "李四")
			if err != nil {
				t.Fatalf("第 %d 轮：投料先成功后重放投料请求应成功: %v", round+1, err)
			}
			assertFeedingRecord(t, *replayFeed, 2, "M1", "2", concurrentNewTime(), "李四")
			if len(mustGetBatch(t, feeder, "B1").Feedings) != 2 {
				t.Fatalf("第 %d 轮：重放投料请求不应新增记录", round+1)
			}

			assertClosedAfterConcurrent(t, dir, closer, feeder, cr.view)
			continue
		}

		// 结果二：关闭先被接受。投料必须以 ErrInvalidState 拒绝。
		closeFirst++
		if !errors.Is(fr.err, ErrInvalidState) {
			t.Fatalf("第 %d 轮：关闭先成功后追加投料应返回 ErrInvalidState，得到 %v", round+1, fr.err)
		}
		if fr.view != nil {
			t.Fatalf("第 %d 轮：被拒绝的投料不应返回投料结果: %+v", round+1, fr.view)
		}

		// 关闭结果只包含关闭前已确认的一条投料。
		if len(cr.view.Feedings) != 1 {
			t.Fatalf("第 %d 轮：关闭先成功时结果应只含 1 条投料，得到 %d 条 %+v",
				round+1, len(cr.view.Feedings), cr.view.Feedings)
		}
		assertFeedingRecord(t, cr.view.Feedings[0], 1, "M1", "4", concurrentOriginalTime(), "张三")
		// 实投不因被拒绝的投料增加：仍为 4，差额 4-10=-6。
		assertReconciliation(t, cr.view, "4", "-6")

		// 被拒绝的投料不能出现在任一句柄随后查到的台账中：不能多出 2 克
		// 记录，不能增加实投，也不能留下空的登记位置（序号只能是 1）。
		for name, s := range map[string]*Store{"关闭方": closer, "投料方": feeder} {
			v := mustGetBatch(t, s, "B1")
			if len(v.Feedings) != 1 || v.Feedings[0].Seq != 1 {
				t.Fatalf("第 %d 轮：%s 台账应只有序号 1 的一条投料，得到 %+v",
					round+1, name, v.Feedings)
			}
			for _, f := range v.Feedings {
				if f.MaterialNo == "M1" && f.Grams == "2" {
					t.Fatalf("第 %d 轮：%s 台账中不应出现被拒绝的 2 克投料: %+v", round+1, name, f)
				}
				if f.Registrar == "李四" {
					t.Fatalf("第 %d 轮：%s 台账中不应出现被拒绝投料的登记人: %+v", round+1, name, f)
				}
			}
			assertReconciliation(t, v, "4", "-6")
		}

		// 失败不占用请求编号：批次已关闭，用同一编号再次提交仍按当前状态
		// 返回 ErrInvalidState，而不是因为编号被占用返回 ErrRequestConflict。
		if _, err := feeder.AddFeeding(concurrentNewFeedNo, "B1", "M1", "2",
			concurrentNewTime(), "李四"); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("第 %d 轮：被拒绝的请求编号应未被占用，再次提交仍应得到 ErrInvalidState，得到 %v",
				round+1, err)
		}

		assertClosedAfterConcurrent(t, dir, closer, feeder, cr.view)
	}
	t.Logf("并发 %d 轮中：投料先被接受 %d 次，关闭先被接受 %d 次", rounds, feedingFirst, closeFirst)
}

// TestFeedingAcceptedBeforeCloseDeterministic 确定性地钉死“投料先被接受”
// 的完整结果：投料方先追加成功，关闭方随后关闭。两方提交前同样都已打开
// 台账并取得过执行中的旧批次记录。关闭结果必须包含原有与新增两条投料，
// 新投料沿用序号 2 且内容完整，M1 实投 6、差额 -4，M2 实投 0、差额 -7。
func TestFeedingAcceptedBeforeCloseDeterministic(t *testing.T) {
	dir := t.TempDir()
	setupConcurrentCloseFeedingLedger(t, dir)
	closer, feeder := openTwoConcurrentHandles(t, dir)
	assertBothSawExecutingBatch(t, closer, feeder)

	added, err := feeder.AddFeeding(concurrentNewFeedNo, "B1", "M1", "2",
		concurrentNewTime(), "李四")
	if err != nil {
		t.Fatalf("投料先提交应成功: %v", err)
	}
	assertFeedingRecord(t, *added, 2, "M1", "2", concurrentNewTime(), "李四")

	// 关闭方持有的是只有一条投料的旧快照，但提交时必须在重新读取到的
	// 最新状态上关闭，不能用旧快照覆盖刚接受的投料。
	closed, err := closer.CloseBatch(concurrentCloseReq, "B1")
	if err != nil {
		t.Fatalf("投料成功后关闭应成功: %v", err)
	}
	assertClosedIdentity(t, closed)
	if len(closed.Feedings) != 2 {
		t.Fatalf("关闭结果应包含原有与新增 2 条投料，得到 %d 条 %+v",
			len(closed.Feedings), closed.Feedings)
	}
	assertFeedingRecord(t, closed.Feedings[0], 1, "M1", "4", concurrentOriginalTime(), "张三")
	assertFeedingRecord(t, closed.Feedings[1], 2, "M1", "2", concurrentNewTime(), "李四")
	assertReconciliation(t, closed, "6", "-4")

	assertClosedAfterConcurrent(t, dir, closer, feeder, closed)
}

// TestCloseAcceptedBeforeFeedingRejectedDeterministic 确定性地钉死“关闭先
// 被接受”的完整结果：关闭方先关闭成功，投料方随后追加。投料必须返回
// ErrInvalidState 且无结果；关闭结果只含关闭前一条投料，M1 实投 4、
// 差额 -6，M2 实投 0、差额 -7；被拒绝的投料不落盘、不增实投、不留空
// 登记位置，且不占用请求编号。
func TestCloseAcceptedBeforeFeedingRejectedDeterministic(t *testing.T) {
	dir := t.TempDir()
	setupConcurrentCloseFeedingLedger(t, dir)
	closer, feeder := openTwoConcurrentHandles(t, dir)
	assertBothSawExecutingBatch(t, closer, feeder)

	closed, err := closer.CloseBatch(concurrentCloseReq, "B1")
	if err != nil {
		t.Fatalf("关闭先提交应成功: %v", err)
	}
	assertClosedIdentity(t, closed)
	if len(closed.Feedings) != 1 {
		t.Fatalf("关闭结果应只含关闭前 1 条投料，得到 %d 条 %+v",
			len(closed.Feedings), closed.Feedings)
	}
	assertFeedingRecord(t, closed.Feedings[0], 1, "M1", "4", concurrentOriginalTime(), "张三")
	assertReconciliation(t, closed, "4", "-6")

	rejected, err := feeder.AddFeeding(concurrentNewFeedNo, "B1", "M1", "2",
		concurrentNewTime(), "李四")
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("关闭后追加投料应返回 ErrInvalidState，得到 %v", err)
	}
	if rejected != nil {
		t.Fatalf("被拒绝的投料不应返回结果: %+v", rejected)
	}

	// 投料方此前持有的是执行中旧快照，随后查询必须重新读取到已关闭现状，
	// 且与关闭结果一致：被拒绝的投料不在台账中。
	assertClosedAfterConcurrent(t, dir, closer, feeder, closed)

	// 失败不占用请求编号、不留空登记位置：再次以同编号提交仍按状态拒绝，
	// 台账中序号只有 1，M1 实投仍为 4。
	again, err := feeder.AddFeeding(concurrentNewFeedNo, "B1", "M1", "2",
		concurrentNewTime(), "李四")
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("被拒绝的请求编号应未被占用，再次提交仍应得到 ErrInvalidState，得到 %v", err)
	}
	if again != nil {
		t.Fatalf("再次被拒绝的投料不应返回结果: %+v", again)
	}
	v := mustGetBatch(t, feeder, "B1")
	if len(v.Feedings) != 1 || v.Feedings[0].Seq != 1 || v.Feedings[0].Grams != "4" {
		t.Fatalf("被拒绝的投料不应留下记录或空登记位置，得到 %+v", v.Feedings)
	}
	assertReconciliation(t, v, "4", "-6")
}
