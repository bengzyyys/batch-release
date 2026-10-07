package release

import (
	"encoding/json"
	"fmt"
)

// batchResultRequest 概括读取台账时四类“返回批次结果”的已保存成功请求
// （创建批次 createBatch、草稿调整 updateDraftBatch、开始执行 startBatch、
// 关闭批次 closeBatch）共有的核对规则。四类结果都必须先通过这里的共同
// 检查，再按各自类别与业务含义继续核对：
//   - 原提交内容必须能解析出批次编号（解析失败时无法确定关联批次，错误
//     只指明请求编号与操作类别）；
//   - 保存结果必须完整：缺失、为 null、为空对象、无法解析为完整批次结果，
//     或读不出批次编号，都不能当作成功结果；
//   - 原请求对应的批次必须仍然存在：批次没有了，保存的结果不能单独作为
//     成功的依据；
//   - 保存结果的批次编号必须就是原请求指定的批次：另一批次即使采用同一
//     配方、份数和投料，其结果也不能顶替。
//
// 共同检查通过并不意味着四类结果可以按同一种现状核对：创建与草稿调整结果
// 仍是当时尚无投料的草稿（草稿快照类的进一步共有规则见
// draftSnapshotRequest），开始执行与关闭结果对应批次状态变更时的内容
// （状态变更类的进一步共有规则见 statusTransitionRequest）；四类各自的
// 业务判断（依据选取、状态、投料与数量核对）仍由 validateCreateBatchRequest、
// validateUpdateDraftRequest、validateStartBatchRequest、
// validateCloseBatchRequest 在共有判定之后分别核对。这些差异不因共用本
// 结构的检查而改变，也不能用当前批次查询结果补齐或替换历史返回结果。
//
// 各 label 字段保留四类操作各自的错误措辞：整理成一份逻辑后，错误仍带
// 操作类别、请求编号、能确定的批次编号及具体不符原因，不会退化成无法
// 定位的通用提示。
type batchResultRequest struct {
	// opLabel 是错误信息中的操作类别（创建批次请求 / 草稿调整请求 /
	// 开始执行请求 / 关闭请求）。
	opLabel string
	// resultLabel 是“保存的xx结果”在错误信息中的叫法（创建结果 /
	// 调整结果 / 开始结果 / 关闭结果）。
	resultLabel string
	// successLabel 说明这是哪一类成功（创建成功 / 调整成功 / 开始成功 /
	// 关闭成功），用于原批次不存在时的错误说明。
	successLabel string
}

// resolveTarget 核对四类请求共有的前半部分规则，成功时返回原请求指定的
// 批次编号、该批次的记录，以及解析后的保存结果视图。核对内容与顺序：
//  1. 原提交内容必须能解析出批次编号；
//  2. 保存结果缺失、为 null、为空对象或无法解析为完整批次结果时拒绝；
//  3. 读不出批次编号的结果不能当作成功结果；
//  4. 原请求对应的批次必须存在。
//
// 结果批次编号与原请求指定批次的一致性核对由 checkOwnership 完成：各类别
// 在它与各自后续业务检查之间的先后关系不同（草稿快照类先核对归属再看
// 快照内容，状态变更类先核对原批次当前状态准入再核对归属），因此归属
// 核对单独成一步，由调用方按各自原有顺序插入，不在此合并。
func (cfg batchResultRequest) resolveTarget(st *persistedState, reqNo string, req *requestRecord) (string, *batchRecord, *BatchView, error) {
	// 四类请求的载荷都以批次编号定位批次，用匿名结构解析即可，不依赖
	// 具体载荷类型。
	var payload struct {
		BatchNo string
	}
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		// 提交内容本身已无法解析时无法确定关联批次，错误信息只指明请求编号
		// 与操作类别。
		return "", nil, nil, fmt.Errorf("%w: %s %q 保存的提交内容无法解析",
			ErrCorruptData, cfg.opLabel, reqNo)
	}
	batchNo := payload.BatchNo
	if len(req.Result) == 0 {
		return "", nil, nil, fmt.Errorf("%w: %s %q（批次 %q）缺少保存的%s",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	var result BatchView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return "", nil, nil, fmt.Errorf("%w: %s %q（批次 %q）保存的%s无法解析为完整批次结果",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	// null、空对象或读不出批次编号的结果都不能当作成功结果。
	if result.BatchNo == "" {
		return "", nil, nil, fmt.Errorf("%w: %s %q（批次 %q）保存的%s缺失或不完整",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel)
	}
	// 原请求对应的批次必须仍然存在：批次没有了，保存的结果不能单独作为
	// 成功的依据。
	b := findBatch(st, batchNo)
	if b == nil {
		return "", nil, nil, fmt.Errorf("%w: %s %q 对应的批次 %q 不存在，保存的%s不能单独作为%s的依据",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.resultLabel, cfg.successLabel)
	}
	return batchNo, b, &result, nil
}

// checkOwnership 确认保存结果的批次编号就是原请求指定的批次：另一批次
// 即使采用同一配方、份数和投料，其结果也不能顶替。这是四类请求共有的
// 核对，但各类别把它放在各自检查序列中的不同位置（见 resolveTarget 的
// 说明），因此单独成一步由调用方按原有顺序调用。
func (cfg batchResultRequest) checkOwnership(reqNo, batchNo string, result *BatchView) error {
	if result.BatchNo != batchNo {
		return fmt.Errorf("%w: %s %q（批次 %q）保存结果的批次编号为 %q，不能用另一批次的结果顶替",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.BatchNo)
	}
	return nil
}
