package release

import (
	"encoding/json"
	"fmt"
)

// batchRequestCheck 概括读取台账时四类“以批次编号定位批次、保存批次结果”的
// 已保存成功请求的共同核对上下文：创建批次 createBatch、草稿调整
// updateDraftBatch、开始执行 startBatch、关闭批次 closeBatch。
//
// 这四类结果无论成功时的业务含义如何，都必须满足同一组归属与完整性规则，
// 这些规则只在本结构核对一份，后续维护时不必分别修改四处：
//  1. 原提交内容必须能解析出批次编号——解析失败时无法确定关联批次，错误只
//     指明请求编号与操作类别；
//  2. 保存结果缺失、为 null、为空对象、无法解析为批次结果，或读不出批次
//     编号时拒绝（没有批次编号就无法确认结果属于原请求指定的批次）；
//  3. 能读出批次编号后，原请求对应的批次必须仍然存在——批次后来的调整、
//     开始执行、投料与关闭都属于批次现状，不影响这份历史结果；
//  4. 保存结果的批次编号必须与原请求指定的批次一致——不能因为另一个批次
//     采用同一配方、份数和投料，就接受它的结果。
//
// 各步骤严格按上述先后报告原因：同一份记录同时有多处问题时，仍报最先能
// 确定的那一处。错误统一保留操作类别（opLabel）、请求编号、能确定的批次
// 编号及具体原因。
//
// 四类结果各自的业务判断（创建结果须对应原提交选定的版本与份数；草稿调整
// 结果须对应当次采用的计划且两者都是无投料草稿；开始结果须是执行中且无
// 投料；关闭结果须保留关闭时确认的全部投料与数量核对）不在这里，由
// draftSnapshotRequest、statusTransitionRequest 及各 validateXxxRequest
// 在共同步骤之后继续核对。
type batchRequestCheck struct {
	// opLabel 是错误信息中的操作类别（创建批次请求 / 草稿调整请求 /
	// 开始执行请求 / 关闭请求）。
	opLabel string
	// resultLabel 是“保存的xx结果”在错误信息中的叫法（创建结果 / 调整
	// 结果 / 开始结果 / 关闭结果）。
	resultLabel string
	// successLabel 说明这是哪一类成功（创建成功 / 调整成功 / 开始成功 /
	// 关闭成功），用于原批次不存在时的错误说明。
	successLabel string
}

// parseRequestBatch 解析原提交内容并取出批次编号。四类请求的提交内容都以
// 批次编号定位批次，用匿名结构解析即可，不依赖具体载荷类型。提交内容无法
// 解析时无法确定关联批次，错误信息只指明请求编号与操作类别，不带批次
// 编号——这一步先于一切结果检查。
func (cfg batchRequestCheck) parseRequestBatch(reqNo string, req *requestRecord) (string, error) {
	var payload struct {
		BatchNo string
	}
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		return "", fmt.Errorf("%w: %s %q 保存的提交内容无法解析",
			ErrCorruptData, cfg.opLabel, reqNo)
	}
	return payload.BatchNo, nil
}

// requireBatchResult 核对保存结果的完整性：缺失、为 null、为空对象、无法
// 解析为批次结果，或读不出批次编号（null、空对象得到的零值）都不能当作
// 成功结果。能读出批次编号是后续“结果属于原请求指定批次”检查的前提，
// 因此在此一并拒绝。batchNo 是已从原提交内容确定的批次编号，用于错误
// 信息定位。
func (cfg batchRequestCheck) requireBatchResult(reqNo, batchNo string, req *requestRecord) (*BatchView, error) {
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
	return &result, nil
}

// requireExistingBatch 核对原请求对应的批次必须仍然存在。批次没有了，保存
// 的结果不能单独作为成功的依据；批次后来的调整、开始执行、投料与关闭都
// 属于现状，不影响这份历史结果。
func (cfg batchRequestCheck) requireExistingBatch(st *persistedState, reqNo, batchNo string) (*batchRecord, error) {
	b := findBatch(st, batchNo)
	if b == nil {
		return nil, fmt.Errorf("%w: %s %q 对应的批次 %q 不存在，保存的%s不能单独作为%s的依据",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel, cfg.successLabel)
	}
	return b, nil
}

// requireResultOwnedByBatch 核对保存结果的批次编号必须就是原请求指定的
// 批次。另一批次即使采用同一配方、份数和投料，其结果也不能顶替。
func (cfg batchRequestCheck) requireResultOwnedByBatch(reqNo, batchNo string, result *BatchView) error {
	if result.BatchNo != batchNo {
		return fmt.Errorf("%w: %s %q（批次 %q）保存结果的批次编号为 %q，不能用另一批次的结果顶替",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.BatchNo)
	}
	return nil
}
