package release

import (
	"encoding/json"
	"fmt"
)

// draftSnapshotRequest 概括读取台账时“草稿快照类”已保存成功请求
// （创建批次 createBatch、草稿调整 updateDraftBatch）的核对上下文。两类结果
// 都是当时尚无投料的草稿批次快照，共有规则都在此核对一份：
//   - 保存结果必须完整：缺失、为 null、为空对象或无法读成完整批次结果都
//     拒绝；
//   - 原请求对应的批次必须仍然存在，保存结果的批次编号必须就是原请求所指
//     的批次——另一批次即使配方、份数相同，其结果也不能顶替；
//   - 结果必须仍是草稿、投料列表为空：批次后来再次调整、开始执行、追加
//     投料或关闭都属于批次现状，不能混入这份快照，也不能使这份当时合法的
//     结果失效。
//
// 两类结果的区别不在这层处理：配方版本与份数的选取依据各自不同——创建结果
// 以原创建请求选定的配方版本与份数为准，名称取那个已登记版本；草稿调整结果
// 保留当次实际采用的计划（请求明确给出新版本或新份数时必须符合指定值，配方
// 编号与版本同时留空、份数传 0 表示沿用，沿用值只须满足完整性与数量规则，
// 不拿批次现在的计划反推）。这些各自依据的核对，以及以各自依据进行的逐物料
// 数量核对（统一经 validateUnfedRequirements），由 validateCreateBatchRequest、
// validateUpdateDraftRequest 在本结构给出的共有判定之后各自完成。
//
// 各 label 字段保留两类操作各自的错误措辞：整理成一份逻辑后，错误仍带
// 操作类别、请求编号、能确定的批次编号及具体不符原因，不会退化成无法
// 定位的通用提示。
type draftSnapshotRequest struct {
	// opLabel 是错误信息中的操作类别（创建批次请求 / 草稿调整请求）。
	opLabel string
	// resultLabel 是“保存的xx结果”在错误信息中的叫法（创建结果 / 调整结果）。
	resultLabel string
	// successLabel 说明这是哪一类成功（创建成功 / 调整成功），用于原批次
	// 不存在时的错误说明。
	successLabel string
	// emptyFeedingsDesc 说明结果为什么应当没有投料（首次创建时 / 调整成功时
	// 的投料列表应为空），用于混入投料时的错误说明。
	emptyFeedingsDesc string
}

// createBatchRequestCheck 是已保存创建批次请求的共有核对配置。
var createBatchRequestCheck = draftSnapshotRequest{
	opLabel:           "创建批次请求",
	resultLabel:       "创建结果",
	successLabel:      "创建成功",
	emptyFeedingsDesc: "首次创建时的投料列表应为空",
}

// updateDraftRequestCheck 是已保存草稿调整请求的共有核对配置。
var updateDraftRequestCheck = draftSnapshotRequest{
	opLabel:           "草稿调整请求",
	resultLabel:       "调整结果",
	successLabel:      "调整成功",
	emptyFeedingsDesc: "调整成功时的投料列表应为空",
}

// payloadUnparseable 是原提交内容无法解析时的统一错误：此时无法确定关联
// 批次，错误信息只指明请求编号与操作类别。
func (cfg draftSnapshotRequest) payloadUnparseable(reqNo string) error {
	return fmt.Errorf("%w: %s %q 保存的提交内容无法解析",
		ErrCorruptData, cfg.opLabel, reqNo)
}

// resolve 核对创建批次/草稿调整两类已保存成功请求的共有规则，成功时返回
// 解析后的保存结果视图。batchNo 是调用方从原提交内容中读出的原请求所指
// 批次编号（两类请求的提交内容结构不同，解析与各自的提交内容合法性检查
// 由调用方完成）。核对内容：
//   - 保存结果缺失、为 null、为空对象或无法解析为完整批次结果时拒绝；
//   - 原请求所指批次必须仍然存在：批次没有了，保存的结果不能单独作为
//     成功的依据；批次后来的调整、执行、投料与关闭都属于现状，不影响
//     这份快照结果的核对；
//   - 保存结果的批次编号必须就是原请求所指的批次，内容相同的另一批次
//     的结果不能顶替；
//   - 结果必须仍是草稿：被改成执行中或已关闭的现状，即与当时的快照
//     记录不一致；
//   - 结果的投料列表必须为空：快照成功那一刻不存在投料，混入批次后来
//     的投料现状即不一致。
//
// 至此共有规则结束：结果采用的配方版本是否仍登记、名称是否一致、份数
// 是否合法及是否符合原请求的指定值或沿用语义，以及逐物料数量核对，都
// 不在这里判断，由调用方按各自选取配方与份数的规则继续核对。
func (cfg draftSnapshotRequest) resolve(st *persistedState, reqNo string, req *requestRecord, batchNo string) (*BatchView, error) {
	if len(req.Result) == 0 {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）缺少保存的%s",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	var result BatchView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）保存的%s无法解析为完整批次结果",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	// null、空对象或读不出批次编号的结果都不能当作成功结果。
	if result.BatchNo == "" {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）保存的%s缺失或不完整",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	// 原请求对应的批次必须仍然存在：批次没有了，保存的结果不能单独作为
	// 成功的依据。批次后来的调整、执行、投料与关闭都属于现状，不影响
	// 这份快照结果的核对。
	if findBatch(st, batchNo) == nil {
		return nil, fmt.Errorf("%w: %s %q 对应的批次 %q 不存在，保存的%s不能单独作为%s的依据",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel, cfg.successLabel)
	}
	// 保存结果必须指向原请求所指的批次：内容相同的另一批次的结果不能顶替。
	if result.BatchNo != batchNo {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果的批次编号为 %q，不能用另一批次的结果顶替",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.BatchNo)
	}
	// 快照成功时的结果必须仍是草稿：被改成执行中或已关闭的现状，即与
	// 当时的快照记录不一致。
	if result.Status != StatusDraft {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果的状态为 %q，不是草稿",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.Status)
	}
	// 快照成功时不存在任何投料：保存结果里出现投料，说明混入了批次后来
	// 开始执行后追加的投料现状，与当时的快照记录不一致。
	if len(result.Feedings) != 0 {
		return nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果包含 %d 条投料，%s，不能混入后来追加的投料",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, len(result.Feedings), cfg.emptyFeedingsDesc)
	}
	return &result, nil
}
