package release

import (
	"fmt"
	"time"
)

// 本文件把“一条投料的内容是否与另一条投料为同一项登记”的核对规则集中到
// 一处：成功投料请求保存结果的核对（validateFeedingRequest：保存结果要同时
// 对应批次中的实际投料、并符合原提交内容）与关闭批次保存结果中逐条投料的
// 核对（validateCloseBatchRequest：按登记顺序逐条对应批次确认的投料）此前
// 各自维护物料、克数、投料时间、登记人的比较，同一业务判断散落在两处。
//
// 两处场景的业务依据不同（投料请求按“原请求指定批次中同序号的实际投料 +
// 原提交内容”两层核对；关闭结果按登记顺序逐条对应，条数、序号、内容均
// 相符），但“两条投料在内容上是否相同”的判断必须完全一致，统一由这里的
// 规则实现，调用方只保留各自的对应关系、定位信息与错误说明。
//
// 统一的投料内容核对规则：
//   - 物料编号精确匹配：逐字符比较，不做大小写或空白归一化，物料不同即不是
//     同一项投料；
//   - 登记人精确匹配：逐字符比较，规则与物料编号相同；
//   - 克数比较实际数量：克数字符串先按千分之一克精确解析，再比较实际数量；
//     1 与 1.000、0.500 与 0.5 表示相同数量，不因末尾的零不同而判为差异；
//     无法解析的克数写法不属于“内容不符”，而是单独的克数非法损坏原因；
//   - 投料时间比较同一时刻：用 time.Equal 判断，不区分时区写法（同一时刻的
//     不同时区表示视为相同），时刻不同即不接受。
//
// 上述四项中任一项不同都不能接受，即使不同投料的克数合计相同也一样；数量
// 的等价写法只用于读取核对，请求内容的幂等匹配仍由写入路径按原文比较，不
// 经过这里。序号不属于投料内容：投料请求场景按保存结果序号在原请求指定批次
// 中定位实际投料，关闭结果场景按登记位置逐条核对序号，均由各调用方处理。

// feedingContent 是投料内容核对使用的统一形态：把保存结果视图（FeedingView）、
// 台账实际投料（feedingRecord）与原提交内容（addFeedingPayload）中需要核对
// 的物料、实际克数、投料时间、登记人四项归一成同一种结构，使任意两种来源
// 都能经同一套规则比较。
//
// grams 是按原文解析出的实际数量（千分之一克）。来自实际投料记录的克数本就
// 是定点数量，不存在写法非法的问题；字符串来源的克数在构造时解析，非法时
// 由 parseFeedingContent 的 ok=false 先行报告，不进入本结构的比较路径。
type feedingContent struct {
	materialNo string
	grams      gramsMilli
	time       time.Time
	registrar  string
}

// parseFeedingContent 把一条以字符串给出克数的投料（成功投料请求的保存结果
// 或原提交内容）归一为 feedingContent。
//
// 克数必须能按正数克数规则精确解析（与登记投料时同一套写法、精度与上限
// 规则）：无法解析时返回 ok=false，由调用方按各自场景报告“克数非法”这一
// 损坏原因——它与字段对得上但取值不同的“内容不符”必须可区分，不能合并成
// 一个泛化的比较失败。1 与 1.000 解析出同一实际数量，在这里不构成差异。
func parseFeedingContent(materialNo, gramsText string, at time.Time, registrar string) (content feedingContent, ok bool) {
	milli, err := parseGrams(gramsText)
	if err != nil {
		return feedingContent{materialNo: materialNo, time: at, registrar: registrar}, false
	}
	return feedingContent{
		materialNo: materialNo,
		grams:      milli,
		time:       at,
		registrar:  registrar,
	}, true
}

// feedingContentFromView 归一成功投料请求保存结果（或关闭结果中某一条投料
// 视图）的内容。
func feedingContentFromView(v FeedingView) (feedingContent, bool) {
	return parseFeedingContent(v.MaterialNo, v.Grams, v.Time, v.Registrar)
}

// feedingContentFromPayload 归一投料请求原提交内容的内容。
func feedingContentFromPayload(p addFeedingPayload) (feedingContent, bool) {
	return parseFeedingContent(p.MaterialNo, p.Grams, p.Time, p.Registrar)
}

// feedingContentFromRecord 归一台账中实际登记的一条投料。实际数量直接取定点
// 存储值，因此不存在克数非法。
func feedingContentFromRecord(r feedingRecord) feedingContent {
	return feedingContent{
		materialNo: r.MaterialNo,
		grams:      r.GramsMilli,
		time:       r.Time,
		registrar:  r.Registrar,
	}
}

// feedingMismatch 标识两份投料内容在哪些字段上不一致。字段命名即核对维度，
// 调用方据此可以说明具体差异而不是只报告一个泛化的比较失败。
type feedingMismatch struct {
	material  bool // 物料编号不同
	grams     bool // 实际克数不同（1 与 1.000 不算不同）
	time      bool // 投料时间不是同一时刻
	registrar bool // 登记人不同
}

// anyMismatch 报告四个维度中是否至少有一项不一致。
func (m feedingMismatch) anyMismatch() bool {
	return m.material || m.grams || m.time || m.registrar
}

// compareFeedingContent 按本文件开头的统一规则判断两份投料内容是否为同一项
// 登记：物料编号与登记人精确匹配，克数比较实际数量，时间比较同一时刻。
//
// 这是纯内容比较，不关心序号、批次归属与条数顺序——那些是各场景的业务
// 依据，由调用方在调用前后自行核对。克数写法在构造 feedingContent 时已经
// 解析，非法写法不会走到这里（由 parseFeedingContent 的 ok=false 先行
// 拒绝），因此本函数返回的只会是“内容不符”，不会与克数非法混淆。
func compareFeedingContent(got, want feedingContent) feedingMismatch {
	return feedingMismatch{
		material:  got.materialNo != want.materialNo,
		grams:     got.grams != want.grams,
		time:      !got.time.Equal(want.time),
		registrar: got.registrar != want.registrar,
	}
}

// feedingGramError 标记关闭结果按登记顺序逐条核对时，某一条投料的克数写法
// 非法：pos 是该投料在列表中的位置（从 1 起），text 是读到的克数原文。
//
// 它与 feedingEntryMismatch 是两类必须可区分的损坏原因：克数写法本身无法
// 解析是克数非法；克数能解析、但取值或物料、时间、登记人与依据不一致（或
// 该位置的序号对不上）是内容不符。调用方不能把两者合并成一个泛化的比较
// 失败。成功投料请求核对的克数非法在其调用点单独报告（不按列表位置定位），
// 不使用本类型。
type feedingGramError struct {
	pos  int
	text string
}

func (e *feedingGramError) Error() string {
	return fmt.Sprintf("克数 %q 不合法", e.text)
}

// feedingEntryMismatch 标记关闭结果按登记顺序逐条核对时，某一条投料与批次
// 实际投料对不上：pos 是该投料在列表中的位置（从 1 起）。无论是序号对不上，
// 还是物料、克数、投料时间、登记人中某项不一致，都属于“内容不符”，与克数
// 非法相区别。具体的场景化错误说明由调用方按原有措辞包装，本类型只承载
// 损坏分类与出错投料位置，避免报告成一个泛化的比较失败。成功投料请求核对
// 不按列表位置定位，其内容不符在调用点直接报告，不使用本类型。
type feedingEntryMismatch struct {
	pos int
}

func (e *feedingEntryMismatch) Error() string {
	return "投料内容与核对依据不一致"
}

// compareCloseFeedings 按关闭结果场景的业务依据，把保存结果中确认的投料
// 逐条与批次实际登记的投料对应：
//   - 条数必须相符：少一条、多一条都不接受，同物料的多次投料不能合并成
//     一条（即使合并后数量合计与分次合计相同）；
//   - 必须按登记顺序逐条对应：以投料在批次列表中的保存位置（第 1 条、第 2
//     条……）一一比较，不能按投料时间重新排序，也不能跳过或调换位置；
//   - 每个位置上序号必须与实际登记序号一致，内容（物料、克数、投料时间、
//     登记人）按 compareFeedingContent 的统一规则核对。
//
// 条数、顺序与序号是关闭结果特有的业务依据，在本函数内核对；“两条投料内容
// 是否相同”的判断与成功投料请求的核对共用 compareFeedingContent，两处不再
// 各自维护。克数非法（*feedingGramError）与内容不符（*feedingEntryMismatch）
// 通过返回错误的具体类型区分，错误中携带出错投料位置（从 1 起），调用方据此
// 包装出带操作、请求编号、批次编号与投料位置的 ErrCorruptData 说明。条数
// 不符不属于单条投料的克数/内容问题，返回普通错误，由调用方保留其原有说明。
//
// got 是保存的关闭结果里的投料视图（按其保存顺序），want 是批次实际登记的
// 投料（按登记顺序）。克数解析先于序号与内容判断，与既有核对顺序一致：序号
// 被改、同时克数也非法时仍报克数非法。
func compareCloseFeedings(got []FeedingView, want []feedingRecord) error {
	if len(got) != len(want) {
		return fmt.Errorf("保存结果的投料条数 %d 与批次实际投料条数 %d 不一致，不能少一条、多一条或合并同物料的记录",
			len(got), len(want))
	}
	for i := range want {
		pos := i + 1
		rec := want[i]
		fv := got[i]
		viewContent, ok := feedingContentFromView(fv)
		if !ok {
			return &feedingGramError{pos: pos, text: fv.Grams}
		}
		// 序号是关闭结果特有的核对维度：按登记位置逐条对应时，该位置的序号
		// 必须就是实际登记序号，不能重排、改号；它与物料、克数、时间、登记人
		// 的任一差异同属“该条投料与实际投料不一致”，沿用同一条场景化说明。
		m := compareFeedingContent(viewContent, feedingContentFromRecord(rec))
		if fv.Seq != rec.Seq || m.anyMismatch() {
			return &feedingEntryMismatch{pos: pos}
		}
	}
	return nil
}
