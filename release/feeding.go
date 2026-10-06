package release

import (
	"fmt"
	"time"
)

// feedingContent 是读取台账时一份待核对的投料内容：物料编号、克数原文、
// 投料时间与登记人。两处读取核对都要判断这样的内容是否就是批次中的
// 某条实际投料：
//   - 已保存的成功投料请求：保存结果（FeedingView）必须对应原请求批次中
//     同序号的实际投料，同时还要符合原提交内容（addFeedingPayload）——
//     两个来源都先整理成本结构，再按同一套规则核对；
//   - 已保存的成功关闭请求：保存结果中的每条投料（FeedingView）必须按
//     登记顺序与批次确认的同位置实际投料一致。
//
// 两种场景的业务依据与错误说明各自保留在调用方（投料请求要符合原提交
// 内容、关闭结果要指出第几条投料），投料内容本身的判断标准统一由本
// 文件实现，不再分别维护。
type feedingContent struct {
	materialNo string
	grams      string
	time       time.Time
	registrar  string
}

// feedingViewContent 取出保存结果中一条投料视图的待核对内容。
func feedingViewContent(fv FeedingView) feedingContent {
	return feedingContent{
		materialNo: fv.MaterialNo,
		grams:      fv.Grams,
		time:       fv.Time,
		registrar:  fv.Registrar,
	}
}

// feedingPayloadContent 取出投料请求原提交内容的待核对内容。
func feedingPayloadContent(p addFeedingPayload) feedingContent {
	return feedingContent{
		materialNo: p.MaterialNo,
		grams:      p.Grams,
		time:       p.Time,
		registrar:  p.Registrar,
	}
}

// parsedFeedingContent 是克数已换算成千分之一克的投料内容，
// 可以直接与另一份已解析内容比较。
type parsedFeedingContent struct {
	materialNo string
	gramsMilli gramsMilli
	time       time.Time
	registrar  string
}

// parse 把克数原文换算成千分之一克，得到可比较的投料内容。换算规则与
// 登记投料时相同（parseGrams）：1 与 1.000 是同一数量。克数写法无法
// 解析时返回错误——“克数不合法”与“内容不符”是两种可区分的损坏原因，
// 这里只负责前者，由调用方按各自场景（保存结果还是原提交内容、第几条
// 投料）包装报告。
func (c feedingContent) parse() (parsedFeedingContent, error) {
	g, err := parseGrams(c.grams)
	if err != nil {
		return parsedFeedingContent{}, fmt.Errorf("克数 %q 不合法", c.grams)
	}
	return parsedFeedingContent{
		materialNo: c.materialNo,
		gramsMilli: g,
		time:       c.time,
		registrar:  c.registrar,
	}, nil
}

// feedingRecordContent 取出批次中一条实际投料记录的已解析内容：
// 记录中的克数本就以千分之一克保存，无需再解析。
func feedingRecordContent(rec feedingRecord) parsedFeedingContent {
	return parsedFeedingContent{
		materialNo: rec.MaterialNo,
		gramsMilli: rec.GramsMilli,
		time:       rec.Time,
		registrar:  rec.Registrar,
	}
}

// matches 是“两份投料内容是否表示同一次投料”的唯一判断：物料编号与
// 登记人精确匹配，克数比较实际数量（1 与 1.000 解析后相同），时间比较
// 同一时刻（不区分时区写法）。实际数量、时间或登记人不同即不表示同一
// 次投料——即使与其他记录的数量合计相同，也不能顶替。该判断只用于读取
// 核对；请求内容的幂等匹配仍由写入路径按提交原文比较（把 1.000 改成 1
// 再提交仍是 ErrRequestConflict），不经过这里。
func (c parsedFeedingContent) matches(other parsedFeedingContent) bool {
	return c.materialNo == other.materialNo &&
		c.gramsMilli == other.gramsMilli &&
		c.time.Equal(other.time) &&
		c.registrar == other.registrar
}
